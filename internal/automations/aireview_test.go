package automations_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/automations"
)

// roundTripper lets the AI reviewer be tested end to end without a network:
// the Groq call goes through the injected *http.Client.
type roundTripper struct {
	mu     sync.Mutex
	calls  []*http.Request
	bodies []string
	reply  func(*http.Request) (*http.Response, error)
}

func (r *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	return r.reply(req)
}

func (r *roundTripper) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *roundTripper) lastBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return ""
	}
	return r.bodies[len(r.bodies)-1]
}

// groqReplying builds a transport that answers every request with the given
// model output (the JSON string the model "wrote").
func groqReplying(content string) *roundTripper {
	return &roundTripper{reply: func(*http.Request) (*http.Response, error) {
		payload, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(payload))),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}}
}

func reviewerWith(rt *roundTripper, candidates []automations.Candidate) (*automations.Automations, *recorder) {
	rec := &recorder{}
	a := automations.New(automations.Config{
		Now:        newTick(june10).fn(),
		HTTPClient: &http.Client{Transport: rt},
		OnAction:   rec.hook(),
		CooldownMs: 300_000,
	})
	a.EnableAIReview("gsk_test", "", func() []automations.Candidate { return candidates })
	return a, rec
}

func suspect(id string, blocked int64) automations.Candidate {
	label := "retry_bug"
	return automations.Candidate{TenantID: id, Label: &label, Blocked: blocked, OK: 1, Features: map[string]any{"cv": 0.02}}
}

// ---- the reviewer is off by default ----------------------------------------

func TestAIReviewIsInertWithoutAKey(t *testing.T) {
	rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":5}]}`)
	a := automations.New(automations.Config{Now: newTick(june10).fn(), HTTPClient: &http.Client{Transport: rt}})

	res := a.RunAIReview()
	if res.Ran {
		t.Fatal("the AI reviewer ran with no Groq key configured")
	}
	if rt.callCount() != 0 {
		t.Fatalf("%d requests were sent to Groq without an API key", rt.callCount())
	}
	if a.State().Rules[3].Kind != "ai (offline)" {
		t.Fatalf("the dashboard advertises the AI rule as %q while it is not configured", a.State().Rules[3].Kind)
	}
}

func TestAIReviewIsInertWithoutACandidateSource(t *testing.T) {
	rt := groqReplying(`{"bans":[]}`)
	a := automations.New(automations.Config{Now: newTick(june10).fn(), HTTPClient: &http.Client{Transport: rt}})
	a.EnableAIReview("gsk_test", "", nil)

	if a.RunAIReview().Ran {
		t.Fatal("the AI reviewer ran with no way to enumerate candidates")
	}
	if rt.callCount() != 0 {
		t.Fatalf("%d requests were sent to Groq with no candidate source", rt.callCount())
	}
}

func TestEnablingAIReviewFlipsTheDashboardLabel(t *testing.T) {
	a, _ := reviewerWith(groqReplying(`{"bans":[]}`), nil)
	if got := a.State().Rules[3].Kind; got != "ai" {
		t.Fatalf("with a key wired the AI rule should read %q, got %q", "ai", got)
	}
}

// ---- guardrail 1: only heavy blockers are eligible --------------------------

func TestOnlyHeavyBlockersAreEvenShownToTheModel(t *testing.T) {
	rt := groqReplying(`{"bans":[{"tenantId":"quiet","minutes":10}]}`)
	a, rec := reviewerWith(rt, []automations.Candidate{
		suspect("quiet", 0),
		suspect("mild", 2), // one short of the threshold
	})

	res := a.RunAIReview()
	if !res.Ran {
		t.Fatal("the reviewer refused to run despite being configured")
	}
	if len(res.Banned) != 0 {
		t.Fatalf("the model banned %v; clients below the block threshold must never even be offered to it", res.Banned)
	}
	if rt.callCount() != 0 {
		t.Fatalf("%d requests were sent to Groq with no eligible suspects — an empty review must cost nothing", rt.callCount())
	}
	if rec.countOf("ai_ban") != 0 {
		t.Fatal("an AI ban was recorded without any eligible suspect")
	}
}

func TestAlreadyBannedClientsAreNotResubmitted(t *testing.T) {
	rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":10}]}`)
	a, _ := reviewerWith(rt, []automations.Candidate{suspect("acme", 50)})
	a.Ban("acme", 300_000, "operator")

	res := a.RunAIReview()
	if len(res.Banned) != 0 {
		t.Fatalf("an already-banned client was re-banned by the model: %v", res.Banned)
	}
	if rt.callCount() != 0 {
		t.Fatalf("%d requests were sent to Groq although the only suspect was already serving a cooldown", rt.callCount())
	}
}

// ---- the happy path --------------------------------------------------------

func TestAIReviewBansAVettedSuspect(t *testing.T) {
	rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":12,"reason":"metronome ignoring 429s"}]}`)
	a, rec := reviewerWith(rt, []automations.Candidate{suspect("acme", 40), suspect("globex", 5)})

	res := a.RunAIReview()
	if !res.Ran || res.Error {
		t.Fatalf("the review did not complete cleanly: %+v", res)
	}
	if len(res.Banned) != 1 || res.Banned[0] != "acme" {
		t.Fatalf("the reviewer banned %v, expected exactly [acme]", res.Banned)
	}
	if got := a.BanRemainingMs("acme"); got != 12*60_000 {
		t.Fatalf("acme's AI ban is %dms, expected the 12 minutes the model asked for", got)
	}
	if got := a.BanRemainingMs("globex"); got != 0 {
		t.Fatalf("globex was offered to the model but not chosen, yet it is banned for %dms", got)
	}

	if rec.countOf("ai_ban") != 1 {
		t.Fatalf("%d ai_ban actions were recorded, expected 1", rec.countOf("ai_ban"))
	}
	act := rec.all()[0]
	if act.Type != "ai_ban" {
		t.Fatalf("a model decision was recorded as %q; rule bans and model bans must stay tellable apart", act.Type)
	}
	if !strings.Contains(act.Message, "metronome ignoring 429s") {
		t.Fatalf("the model's reason was dropped from the audit trail: %q", act.Message)
	}
	if act.CooldownMs != 12*60_000 {
		t.Fatalf("the recorded action says %dms, the applied ban says 12 minutes", act.CooldownMs)
	}
}

func TestAIReviewRequestCarriesTheKeyModelAndSuspects(t *testing.T) {
	rt := groqReplying(`{"bans":[]}`)
	rec := &recorder{}
	a := automations.New(automations.Config{
		Now: newTick(june10).fn(), HTTPClient: &http.Client{Transport: rt}, OnAction: rec.hook(),
	})
	a.EnableAIReview("gsk_secret", "llama-custom", func() []automations.Candidate {
		return []automations.Candidate{suspect("acme", 40)}
	})

	a.RunAIReview()
	if rt.callCount() != 1 {
		t.Fatalf("the reviewer made %d Groq calls for one eligible suspect", rt.callCount())
	}

	rt.mu.Lock()
	req := rt.calls[0]
	rt.mu.Unlock()
	if got := req.Header.Get("Authorization"); got != "Bearer gsk_secret" {
		t.Fatalf("the Groq request authenticated as %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("the Groq request declared content-type %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(rt.lastBody()), &body); err != nil {
		t.Fatalf("the request body was not valid JSON: %v", err)
	}
	if body["model"] != "llama-custom" {
		t.Fatalf("the configured model was ignored; the request asked for %v", body["model"])
	}
	if rf, ok := body["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Fatalf("strict JSON mode was not requested: %v", body["response_format"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("the prompt has %d messages, expected a system rule plus the suspect payload", len(msgs))
	}
	user, _ := msgs[1].(map[string]any)
	if !strings.Contains(user["content"].(string), "acme") {
		t.Fatalf("the eligible suspect never reached the model: %v", user["content"])
	}
}

func TestAIReviewFallsBackToADefaultModel(t *testing.T) {
	rt := groqReplying(`{"bans":[]}`)
	a, _ := reviewerWith(rt, []automations.Candidate{suspect("acme", 40)})
	a.RunAIReview()

	var body map[string]any
	_ = json.Unmarshal([]byte(rt.lastBody()), &body)
	if body["model"] == "" || body["model"] == nil {
		t.Fatalf("no model was named in the request: %v", body["model"])
	}
}

// ---- guardrail 2: the model may only pick from the vetted set ---------------

func TestHallucinatedOrInjectedTenantsAreDiscarded(t *testing.T) {
	// If a client can get its own text into the prompt, this is the line that
	// stops it naming a competitor. Only ids we vetted may be banned.
	rt := groqReplying(`{"bans":[
		{"tenantId":"acme","minutes":5,"reason":"real"},
		{"tenantId":"a-tenant-that-was-never-a-suspect","minutes":60,"reason":"injected"},
		{"tenantId":"","minutes":60,"reason":"empty id"}
	]}`)
	a, rec := reviewerWith(rt, []automations.Candidate{suspect("acme", 40)})

	res := a.RunAIReview()
	if len(res.Banned) != 1 || res.Banned[0] != "acme" {
		t.Fatalf("the reviewer banned %v; only the vetted suspect may be acted on", res.Banned)
	}
	if got := a.BanRemainingMs("a-tenant-that-was-never-a-suspect"); got != 0 {
		t.Fatalf("a tenant the model invented was banned for %dms", got)
	}
	if got := a.BanRemainingMs(""); got != 0 {
		t.Fatalf("a ban with an empty tenant id was applied (%dms)", got)
	}
	if rec.countOf("ai_ban") != 1 {
		t.Fatalf("%d ai_ban actions were recorded for one legitimate ban", rec.countOf("ai_ban"))
	}
}

// ---- guardrail 3: the sentence is clamped -----------------------------------

func TestBanLengthIsClamped(t *testing.T) {
	cases := []struct {
		name    string
		minutes string
		wantMs  int64
	}{
		{"an absurd sentence is capped at an hour", "10080", 60 * 60_000},
		{"exactly the cap is honoured", "60", 60 * 60_000},
		{"a normal sentence is honoured", "7", 7 * 60_000},
		{"a sub-minute sentence is raised to one minute", "0.2", 60_000},
		{"zero is raised to one minute", "0", 60_000},
		{"a negative sentence is raised to one minute", "-30", 60_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":` + tc.minutes + `}]}`)
			a, _ := reviewerWith(rt, []automations.Candidate{suspect("acme", 40)})

			a.RunAIReview()
			if got := a.BanRemainingMs("acme"); got != tc.wantMs {
				t.Fatalf("the model asked for %s minutes and got a %dms ban; the clamp must keep it in [1min, 60min] (expected %dms)", tc.minutes, got, tc.wantMs)
			}
		})
	}
}

func TestAMissingReasonStillProducesAnHonestMessage(t *testing.T) {
	rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":5}]}`)
	a, rec := reviewerWith(rt, []automations.Candidate{suspect("acme", 40)})
	a.RunAIReview()

	act := rec.all()[0]
	if !strings.Contains(act.Message, "suspected abuse") {
		t.Fatalf("a ban with no stated reason reads %q; it must still say something truthful", act.Message)
	}
}

// ---- failure paths ---------------------------------------------------------

func TestAIReviewSurvivesEveryFailureMode(t *testing.T) {
	cases := []struct {
		name string
		rt   *roundTripper
	}{
		{"the network is down", &roundTripper{reply: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: no route to host")
		}}},
		{"the API returns something that is not JSON", &roundTripper{reply: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<html>502 Bad Gateway</html>"))}, nil
		}}},
		{"the API returns no choices", &roundTripper{reply: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
		}}},
		{"the model ignores the JSON instruction", groqReplying("Sure! I think you should ban acme.")},
		{"the model returns JSON with no bans key", groqReplying(`{}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, rec := reviewerWith(tc.rt, []automations.Candidate{suspect("acme", 40)})

			res := a.RunAIReview() // must not panic
			if !res.Ran {
				t.Fatal("a review that reached the model reported that it never ran")
			}
			if len(res.Banned) != 0 {
				t.Fatalf("a failed review still banned %v", res.Banned)
			}
			if got := a.BanRemainingMs("acme"); got != 0 {
				t.Fatalf("a failed review left acme banned for %dms — a broken model must never ban anybody", got)
			}
			if rec.countOf("ai_ban") != 0 {
				t.Fatalf("%d ai_ban actions were recorded by a failed review", rec.countOf("ai_ban"))
			}
		})
	}
}

func TestAValidButEmptyModelAnswerIsNotAnError(t *testing.T) {
	rt := groqReplying(`{"bans":[]}`)
	a, _ := reviewerWith(rt, []automations.Candidate{suspect("acme", 40)})

	res := a.RunAIReview()
	if !res.Ran || res.Error || len(res.Banned) != 0 {
		t.Fatalf("the model declining to ban anybody was reported as %+v; that is the healthy, conservative answer", res)
	}
}

// ---- concurrency -----------------------------------------------------------

func TestAIReviewIsSafeAlongsideLiveTraffic(t *testing.T) {
	// The review loop runs on its own ticker while requests keep flowing. Run
	// with -race: the ban map and the action log are shared between them.
	rt := groqReplying(`{"bans":[{"tenantId":"acme","minutes":5,"reason":"abuse"}]}`)
	a, _ := reviewerWith(rt, []automations.Candidate{suspect("acme", 40), suspect("globex", 40)})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 50; i++ {
				a.RunAIReview()
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				_ = a.BanRemainingMs("acme")
				_ = a.State()
				_ = a.Recent(10)
			}
		}()
	}
	close(start)
	wg.Wait()

	// The ban may have been re-applied many times, but it must be coherent.
	if got := a.BanRemainingMs("acme"); got != 5*60_000 && got != 0 {
		t.Fatalf("acme's ban is %dms after concurrent reviews; it should be either the 5-minute sentence or already reaped", got)
	}
}
