package livetest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	srv    *httptest.Server
	calls  int
	status int
	inputs map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{status: http.StatusNoContent}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if r.Header.Get("Authorization") != "Bearer tok" || !strings.HasSuffix(r.URL.Path, "/actions/workflows/xdp-live.yml/dispatches") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Inputs map[string]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.inputs = body.Inputs
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// rewrite sends api.github.com calls to the fake.
type rewrite struct{ to string }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(rw.to, "http://")
	return http.DefaultTransport.RoundTrip(r2)
}

func newRunner(f *fakeGitHub, now *time.Time) *Runner {
	return New(Config{
		GitHubToken: "tok", Repo: "o/r", IngestKey: "k", PublicURL: "https://gw", DailyCap: 2,
		HTTP: &http.Client{Transport: rewrite{f.srv.URL}},
		Now:  func() time.Time { return *now },
	})
}

func TestStartDispatchesOnceAndHoldsTheSlot(t *testing.T) {
	f := newFakeGitHub(t)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	r := newRunner(f, &now)

	run, err := r.Start(context.Background(), "admin@x")
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || f.inputs["run_id"] != run.ID || f.inputs["report_to"] != "https://gw" {
		t.Fatalf("calls=%d inputs=%v", f.calls, f.inputs)
	}
	if _, err := r.Start(context.Background(), "admin@x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start while running: err = %v, want ErrBusy", err)
	}
	if f.calls != 1 {
		t.Fatal("a busy start must not reach GitHub")
	}
}

func TestIngestNeedsTheKeyAndTheCurrentRun(t *testing.T) {
	f := newFakeGitHub(t)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	r := newRunner(f, &now)
	run, _ := r.Start(context.Background(), "a")

	if err := r.Ingest("wrong", run.ID, []string{"x"}, ""); !errors.Is(err, ErrBadKey) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := r.Ingest("k", "someone-else", []string{"x"}, ""); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("other run: %v", err)
	}
	if err := r.Ingest("k", run.ID, []string{"## 0. Who is who"}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Current()
	if got.Status != "running" || got.Lines[len(got.Lines)-1] != "## 0. Who is who" {
		t.Fatalf("status=%s lines=%v", got.Status, got.Lines)
	}
	if err := r.Ingest("k", run.ID, nil, "done"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Current(); got.Status != "done" {
		t.Fatalf("status = %s, want done", got.Status)
	}
}

func TestDailyCapAndFailedDispatchRefund(t *testing.T) {
	f := newFakeGitHub(t)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	r := newRunner(f, &now)

	f.status = http.StatusUnprocessableEntity
	if _, err := r.Start(context.Background(), "a"); err == nil {
		t.Fatal("a refused dispatch must be an error")
	}
	f.status = http.StatusNoContent
	for i := 0; i < 2; i++ { // the refused one did not count
		run, err := r.Start(context.Background(), "a")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		_ = r.Ingest("k", run.ID, nil, "done")
	}
	if _, err := r.Start(context.Background(), "a"); !errors.Is(err, ErrDailyCap) {
		t.Fatalf("third run: %v, want ErrDailyCap", err)
	}
	if left := r.RunsLeft(); left != 0 {
		t.Fatalf("RunsLeft = %d after the cap, want 0", left)
	}
	now = now.Add(24 * time.Hour)
	if left := r.RunsLeft(); left != 2 {
		t.Fatalf("RunsLeft = %d on a new day, want 2", left)
	}
	if _, err := r.Start(context.Background(), "a"); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestANoShowRunFreesTheSlot(t *testing.T) {
	f := newFakeGitHub(t)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	r := newRunner(f, &now)
	if _, err := r.Start(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	if got, _ := r.Current(); got.Status != "failed" {
		t.Fatalf("status = %s, want failed after no report", got.Status)
	}
	if _, err := r.Start(context.Background(), "a"); err != nil {
		t.Fatalf("slot should be free again: %v", err)
	}
}
