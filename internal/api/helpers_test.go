package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/pyjeebz/toss/internal/hub"
)

// newTestServer builds a Server with logging discarded.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(hub.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}
