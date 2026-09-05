package k8s

import (
	"io"
	"log/slog"
)

// discardLogger keeps the expected warnings (410 recovery, watch restarts) out
// of the test output, so a failure is easy to spot.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
