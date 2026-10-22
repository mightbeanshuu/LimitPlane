// Package livetest runs the XDP live test on demand: an admin presses a button,
// the gateway asks GitHub Actions to run .github/workflows/xdp-live.yml, the
// workflow builds two Azure VMs, attacks one from the other, streams every log
// line back here, and deletes the VMs.
//
// The gateway holds no cloud credentials. It can only ask GitHub to start one
// workflow (a fine-grained token with Actions: write on one repository), and
// the workflow logs in to Azure through OIDC with a role scoped to a single
// resource group. What lives here is the bookkeeping: one run at a time, a
// daily cap so a leaked admin password cannot spend the Azure credit, and the
// log buffer the page polls.
package livetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	maxLines = 2000
	// A run that has not reported for this long is over, whatever it last said:
	// the workflow's own timeout is 25 minutes.
	staleAfter = 30 * time.Minute
	// Dispatched but never heard from. GitHub usually starts a runner within a
	// minute; three means it is not coming.
	noShowAfter = 3 * time.Minute
)

var (
	ErrBusy       = errors.New("a live test is already running")
	ErrDailyCap   = errors.New("today's live tests are used up")
	ErrBadKey     = errors.New("bad ingest key")
	ErrUnknownRun = errors.New("not the current run")
)

type Config struct {
	GitHubToken string // fine-grained, Actions: write on Repo only
	Repo        string // owner/name
	Workflow    string // file name under .github/workflows
	Ref         string // branch the workflow runs from
	IngestKey   string // shared with the workflow as a GitHub secret
	PublicURL   string // where the workflow posts its lines back
	DailyCap    int
	HTTP        *http.Client
	Now         func() time.Time
}

// Run is what the page renders.
type Run struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"` // dispatched, running, done, failed
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	StartedBy  string    `json:"startedBy"`
	Lines      []string  `json:"lines"`
	ActionsURL string    `json:"actionsUrl"`
}

type Runner struct {
	cfg Config

	mu       sync.Mutex
	current  *Run
	day      string
	runsUsed int
}

func New(cfg Config) *Runner {
	if cfg.Workflow == "" {
		cfg.Workflow = "xdp-live.yml"
	}
	if cfg.Ref == "" {
		cfg.Ref = "main"
	}
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = 6
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg}
}

// Configured reports whether the button can work at all. Without a token the
// page still replays the last run it heard about, so it is never blank.
func (r *Runner) Configured() bool {
	return r != nil && r.cfg.GitHubToken != "" && r.cfg.Repo != "" && r.cfg.IngestKey != ""
}

// Start claims the single run slot and asks GitHub to run the workflow.
func (r *Runner) Start(ctx context.Context, who string) (Run, error) {
	now := r.cfg.Now()

	r.mu.Lock()
	r.expireLocked(now)
	if r.current != nil && (r.current.Status == "dispatched" || r.current.Status == "running") {
		r.mu.Unlock()
		return Run{}, ErrBusy
	}
	if d := now.UTC().Format("2006-01-02"); d != r.day {
		r.day, r.runsUsed = d, 0
	}
	if r.runsUsed >= r.cfg.DailyCap {
		r.mu.Unlock()
		return Run{}, ErrDailyCap
	}
	id := newID()
	run := &Run{
		ID: id, Status: "dispatched", StartedAt: now, UpdatedAt: now, StartedBy: who,
		Lines:      []string{fmt.Sprintf("## Asking GitHub Actions to start run %s", id)},
		ActionsURL: "https://github.com/" + r.cfg.Repo + "/actions/workflows/" + r.cfg.Workflow,
	}
	// Claim the slot before the network call, so two quick clicks cannot both
	// dispatch. A failed dispatch hands the slot (and the day's count) back.
	r.current = run
	r.runsUsed++
	r.mu.Unlock()

	if err := r.dispatch(ctx, id); err != nil {
		r.mu.Lock()
		run.Status = "failed"
		run.Lines = append(run.Lines, "GitHub refused the dispatch: "+err.Error())
		run.UpdatedAt = r.cfg.Now()
		r.runsUsed--
		out := snapshot(run)
		r.mu.Unlock()
		return out, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshot(run), nil
}

func (r *Runner) dispatch(ctx context.Context, id string) error {
	body, _ := json.Marshal(map[string]any{
		"ref":    r.cfg.Ref,
		"inputs": map[string]string{"run_id": id, "report_to": r.cfg.PublicURL},
	})
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/%s/dispatches", r.cfg.Repo, r.cfg.Workflow)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %s", res.Status)
	}
	return nil
}

// Ingest appends lines the workflow sent. The key is compared in constant time
// for the same reason the Stripe signature is: a timing-leaky compare can be
// guessed one byte at a time.
func (r *Runner) Ingest(key, runID string, lines []string, status string) error {
	if r.cfg.IngestKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(r.cfg.IngestKey)) != 1 {
		return ErrBadKey
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.ID != runID {
		return ErrUnknownRun
	}
	run := r.current
	run.Lines = append(run.Lines, lines...)
	if over := len(run.Lines) - maxLines; over > 0 {
		run.Lines = append([]string{"(earlier lines trimmed)"}, run.Lines[over+1:]...)
	}
	switch status {
	case "done", "failed":
		run.Status = status
	default:
		if run.Status == "dispatched" {
			run.Status = "running"
		}
	}
	run.UpdatedAt = r.cfg.Now()
	return nil
}

// Current is the run the page shows: the live one, or the last one finished.
func (r *Runner) Current() (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.cfg.Now())
	if r.current == nil {
		return Run{}, false
	}
	return snapshot(r.current), true
}

func (r *Runner) expireLocked(now time.Time) {
	run := r.current
	if run == nil {
		return
	}
	switch {
	case run.Status == "dispatched" && now.Sub(run.UpdatedAt) > noShowAfter:
		run.Status = "failed"
		run.Lines = append(run.Lines, "The workflow never reported back. Check GitHub Actions.")
	case run.Status == "running" && now.Sub(run.UpdatedAt) > staleAfter:
		run.Status = "failed"
		run.Lines = append(run.Lines, "The workflow went quiet. Its teardown step still deletes the VMs.")
	}
}

func snapshot(run *Run) Run {
	out := *run
	out.Lines = append([]string(nil), run.Lines...)
	return out
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
