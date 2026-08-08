package handlers

import (
	"bytes"
	"log/slog"
	"sync"
)

// syncBuffer is a bytes.Buffer safe for concurrent Write/String. Needed
// because a couple of the handlers under test (vault-setup-retry) launch a
// background goroutine that keeps logging through slog after the handler
// itself returns — a plain bytes.Buffer would race between that goroutine's
// writes and the test reading the buffer's content.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureAuditLog swaps the default slog logger for one writing into a
// buffer for the duration of fn, then restores it. Used to assert that
// audit.Log actually fired — audit.LogActor writes through slog.Info, there's
// no other hook to observe it from outside the audit package.
func captureAuditLog(fn func()) string {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	defer slog.SetDefault(prev)

	fn()

	return buf.String()
}
