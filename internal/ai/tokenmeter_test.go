package ai

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestEstimateUnits(t *testing.T) {
	tests := []struct {
		tokens int
		want   float64
	}{
		{tokens: 0, want: 1},   // never free: the call itself is what we ration
		{tokens: 1, want: 1},   //
		{tokens: 99, want: 1},  //
		{tokens: 100, want: 1}, // exactly one unit's worth
		{tokens: 101, want: 2}, // rounds up
		{tokens: 250, want: 3},
		{tokens: 1000, want: 10},
		{tokens: -5, want: 1}, // nonsense input still costs the minimum
	}
	for _, tc := range tests {
		if got := EstimateUnits(tc.tokens); got != tc.want {
			t.Errorf("EstimateUnits(%d) = %v, want %v", tc.tokens, got, tc.want)
		}
	}
}

func TestDollarsFor(t *testing.T) {
	tests := []struct {
		name    string
		in, out int
		want    float64
	}{
		{name: "zero", in: 0, out: 0, want: 0},
		{name: "one million input", in: 1_000_000, out: 0, want: 0.59},
		{name: "one million output", in: 0, out: 1_000_000, want: 0.79},
		{name: "mixed", in: 1000, out: 2000, want: 0.00059 + 0.00158},
		{name: "output costs more than input", in: 0, out: 1000, want: 0.00079},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DollarsFor(tc.in, tc.out)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("DollarsFor(%d, %d) = %v, want %v", tc.in, tc.out, got, tc.want)
			}
		})
	}
	// Output tokens are dearer, so the same token total split toward output must
	// cost more. This is what catches the two rates being swapped.
	if DollarsFor(0, 2000) <= DollarsFor(2000, 0) {
		t.Error("output tokens must be priced above input tokens")
	}
}

func TestCompleteOffline(t *testing.T) {
	m := NewTokenMeter("", "", nil)
	if m.Live() {
		t.Fatal("Live() = true with no API key")
	}

	tests := []struct {
		name      string
		prompt    string
		maxTokens int
		wantIn    int
		wantOut   int
	}{
		{name: "empty prompt", prompt: "", maxTokens: 256, wantIn: 0, wantOut: 80},
		{name: "four chars is one token", prompt: "abcd", maxTokens: 256, wantIn: 1, wantOut: 80},
		{name: "rounds up", prompt: "abcde", maxTokens: 256, wantIn: 2, wantOut: 80},
		{name: "ten chars", prompt: strings.Repeat("a", 10), maxTokens: 256, wantIn: 3, wantOut: 80},
		{name: "zero maxTokens means the default", prompt: "abcd", maxTokens: 0, wantIn: 1, wantOut: 80},
		{name: "negative maxTokens means the default", prompt: "abcd", maxTokens: -9, wantIn: 1, wantOut: 80},
		{name: "small maxTokens caps the output", prompt: "abcd", maxTokens: 10, wantIn: 1, wantOut: 10},
		{name: "long prompt", prompt: strings.Repeat("x", 4000), maxTokens: 256, wantIn: 1000, wantOut: 80},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Complete(tc.prompt, tc.maxTokens)
			if err != nil {
				t.Fatalf("Complete() error = %v, want nil offline", err)
			}
			if !got.Simulated {
				t.Error("Simulated = false; an offline estimate must say so")
			}
			if got.Text != simulatedText {
				t.Errorf("Text = %q, want the demo notice", got.Text)
			}
			total := tc.wantIn + tc.wantOut
			want := Usage{InTokens: tc.wantIn, OutTokens: tc.wantOut, TotalTokens: total}
			if got.Usage != want {
				t.Errorf("Usage = %+v, want %+v", got.Usage, want)
			}
			if got.Units != EstimateUnits(total) {
				t.Errorf("Units = %v, want %v", got.Units, EstimateUnits(total))
			}
			if got.USD != round6(DollarsFor(tc.wantIn, tc.wantOut)) {
				t.Errorf("USD = %v, want %v", got.USD, round6(DollarsFor(tc.wantIn, tc.wantOut)))
			}
		})
	}
}

func TestCompleteOfflineIsDeterministic(t *testing.T) {
	m := NewTokenMeter("", "", nil)
	first, _ := m.Complete("how many units is this?", 256)
	second, _ := m.Complete("how many units is this?", 256)
	if first != second {
		t.Errorf("the offline estimate must be reproducible:\n%+v\n%+v", first, second)
	}
	// 23 chars -> ceil(23/4) = 6 in, 80 out, 86 total -> 1 unit.
	if first.Usage.InTokens != 6 || first.Units != 1 {
		t.Errorf("unexpected estimate: %+v", first)
	}
	// (6*0.59 + 80*0.79)/1e6 = 6.674e-5, rounded to six places.
	if first.USD != 0.000067 {
		t.Errorf("USD = %v, want 0.000067", first.USD)
	}
}

func TestCompleteLive(t *testing.T) {
	tests := []struct {
		name      string
		usage     string
		wantUsage Usage
	}{
		{
			name:      "full usage block",
			usage:     `{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165}`,
			wantUsage: Usage{InTokens: 120, OutTokens: 45, TotalTokens: 165},
		},
		{
			name:      "missing total is derived",
			usage:     `{"prompt_tokens":10,"completion_tokens":7}`,
			wantUsage: Usage{InTokens: 10, OutTokens: 7, TotalTokens: 17},
		},
		{
			name:      "an explicit zero total is respected, not derived",
			usage:     `{"prompt_tokens":10,"completion_tokens":7,"total_tokens":0}`,
			wantUsage: Usage{InTokens: 10, OutTokens: 7, TotalTokens: 0},
		},
		{
			name:      "no usage block at all bills zero",
			usage:     `null`,
			wantUsage: Usage{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  hello  "}}],"usage":` + tc.usage + `}`))
			}))
			defer srv.Close()

			m := NewTokenMeter("sk-test", "", clientFor(t, srv))
			got, err := m.Complete("hi", 64)
			if err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if got.Text != "hello" {
				t.Errorf("Text = %q, want the trimmed completion", got.Text)
			}
			if got.Simulated {
				t.Error("Simulated = true on a live call")
			}
			if got.Usage != tc.wantUsage {
				t.Errorf("Usage = %+v, want %+v", got.Usage, tc.wantUsage)
			}
			if got.Units != EstimateUnits(tc.wantUsage.TotalTokens) {
				t.Errorf("Units = %v, want %v", got.Units, EstimateUnits(tc.wantUsage.TotalTokens))
			}
			wantUSD := round6(DollarsFor(tc.wantUsage.InTokens, tc.wantUsage.OutTokens))
			if got.USD != wantUSD {
				t.Errorf("USD = %v, want %v", got.USD, wantUSD)
			}
		})
	}
}

func TestCompleteLiveRequestShape(t *testing.T) {
	var (
		mu     sync.Mutex
		gotHdr http.Header
		gotReq map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode outbound body: %v", err)
		}
		mu.Lock()
		gotHdr, gotReq = r.Header.Clone(), body
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	// Deliberately longer than the cap, and multi-byte, so truncation has to
	// count characters rather than bytes.
	prompt := strings.Repeat("é", 5000)
	m := NewTokenMeter("sk-test", "test-model", clientFor(t, srv))
	if _, err := m.Complete(prompt, 99); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if auth := gotHdr.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", auth)
	}
	if gotReq["model"] != "test-model" {
		t.Errorf("model = %v", gotReq["model"])
	}
	if gotReq["max_tokens"] != float64(99) {
		t.Errorf("max_tokens = %v, want 99", gotReq["max_tokens"])
	}
	if _, sent := gotReq["temperature"]; sent {
		t.Error("the proxy must not invent a temperature the caller did not ask for")
	}

	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v, want user", msg["role"])
	}
	content, _ := msg["content"].(string)
	if n := len([]rune(content)); n != maxPromptChars {
		t.Errorf("prompt sent as %d characters, want it truncated to %d", n, maxPromptChars)
	}
	if !utf8.ValidString(content) || strings.Trim(content, "é") != "" {
		t.Error("truncation split a multi-byte rune instead of cutting between runes")
	}
}

func TestCompleteLiveErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "server error", status: 500, body: `{"error":"boom"}`},
		{name: "unauthorized", status: 401, body: `{"error":"bad key"}`},
		{name: "malformed json", status: 200, body: `{"choices":`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			m := NewTokenMeter("sk-super-secret", "", clientFor(t, srv))
			got, err := m.Complete("hi", 0)
			if err == nil {
				t.Fatal("Complete() error = nil; a billing path must never invent a cost")
			}
			if got != (Completion{}) {
				t.Errorf("Complete() returned %+v alongside an error, want the zero value", got)
			}
			if strings.Contains(err.Error(), "sk-super-secret") {
				t.Errorf("the error leaked the API key: %v", err)
			}
			if !strings.Contains(err.Error(), "token meter") {
				t.Errorf("error is not attributed to this component: %v", err)
			}
		})
	}
}

func TestCompleteLiveNetworkError(t *testing.T) {
	m := NewTokenMeter("sk-test", "", &http.Client{Transport: errTransport{}})
	if _, err := m.Complete("hi", 0); err == nil {
		t.Fatal("Complete() error = nil on a dead network")
	}
}

func TestNewTokenMeterDefaults(t *testing.T) {
	m := NewTokenMeter("", "", nil)
	if m.model != defaultModel {
		t.Errorf("model = %q, want %q", m.model, defaultModel)
	}
	if m.hc == nil || m.hc.Timeout == 0 {
		t.Error("a nil client must be replaced by one that has a timeout")
	}
}

func TestRound6(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{in: 0, want: 0},
		{in: 0.0000649, want: 0.000065},
		{in: 0.00006497, want: 0.000065},
		{in: 0.1234564, want: 0.123456},
		{in: 1.5, want: 1.5},
	}
	for _, tc := range tests {
		if got := round6(tc.in); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("round6(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTruncateChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "shorter than the cap", in: "hello", max: 10, want: "hello"},
		{name: "exactly the cap", in: "hello", max: 5, want: "hello"},
		{name: "ascii is cut", in: "hello", max: 3, want: "hel"},
		{name: "multi-byte is cut on rune boundaries", in: "ééééé", max: 3, want: "ééé"},
		{name: "long bytes but few runes stays whole", in: "ééé", max: 4, want: "ééé"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateChars(tc.in, tc.max); got != tc.want {
				t.Errorf("truncateChars(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}
