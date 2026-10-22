package server

import (
	"errors"
	"net/http"

	"github.com/mightbeanshuu/limitplane/internal/gateway"
	"github.com/mightbeanshuu/limitplane/internal/livetest"
)

// ---- the XDP live test: a button that builds, attacks and deletes two VMs ----
//
// Starting a run is admin-only and costs real cloud credit, so it also sits
// behind livetest's one-at-a-time slot and daily cap. Reading the current run
// is public: the log is redacted at the source, and it lets an interviewer
// watch without an account. Ingest is the workflow's write path, guarded by a
// shared key rather than a user token because nobody logs in from CI.

func (s *Server) handleLiveTestStart(w http.ResponseWriter, r *http.Request) {
	claims := s.requireRole(w, r, "admin")
	if claims == nil {
		return
	}
	if !s.LiveTest.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "live_test_not_configured",
			"message": "Set LIVE_TEST_GITHUB_TOKEN and LIVE_TEST_INGEST_KEY on the gateway.",
		})
		return
	}
	s.startLiveTest(w, r, claims.Sub)
}

// handleLiveTestStartPublic is the same start for anyone, recorded by address.
func (s *Server) handleLiveTestStartPublic(w http.ResponseWriter, r *http.Request) {
	if !s.LiveTest.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "live_test_not_configured", "message": "The live test is not set up on this gateway.",
		})
		return
	}
	s.startLiveTest(w, r, "anon:"+gateway.ClientIP(r))
}

func (s *Server) startLiveTest(w http.ResponseWriter, r *http.Request, who string) {
	run, err := s.LiveTest.Start(r.Context(), who)
	switch {
	case errors.Is(err, livetest.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "busy", "message": "A test is already running. You can watch it here."})
	case errors.Is(err, livetest.ErrDailyCap):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "daily_cap", "message": "Today's live tests are used up. The recorded run is still here to watch."})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "dispatch_failed", "message": err.Error(), "run": run})
	default:
		writeJSON(w, http.StatusAccepted, run)
	}
}

func (s *Server) handleLiveTestIngest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID  string   `json:"runId"`
		Lines  []string `json:"lines"`
		Status string   `json:"status"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	err := s.LiveTest.Ingest(r.Header.Get("X-Ingest-Key"), body.RunID, body.Lines, body.Status)
	switch {
	case errors.Is(err, livetest.ErrBadKey):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad_key"})
	case errors.Is(err, livetest.ErrUnknownRun):
		writeJSON(w, http.StatusGone, map[string]any{"error": "unknown_run"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (s *Server) handleLiveTestGet(w http.ResponseWriter, r *http.Request) {
	run, ok := s.LiveTest.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":    s.LiveTest.Configured(),
		"runsLeftToday": s.LiveTest.RunsLeft(),
		"run":           map[bool]any{true: run, false: nil}[ok],
	})
}
