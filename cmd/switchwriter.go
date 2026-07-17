package cmd

import (
	"bytes"
	"io"
	"sync"

	"github.com/jhoblitt/rooket/internal/run"
)

// switchWriter buffers writes until Promote hands it a live destination,
// then flushes the backlog (under a banner) and streams everything after
// directly. It lets cluster create run concurrently with make: make owns
// the terminal while it runs, and create's output appears — in order and
// then live — the moment make's stream ends. Live streaming after
// promotion matters because create can sit for minutes in apt retries or
// device waits; a dump-only-at-join design would show nothing during that.
type switchWriter struct {
	mu     sync.Mutex
	dst    io.Writer
	buf    bytes.Buffer
	banner string
}

func newSwitchWriter(banner string) *switchWriter {
	return &switchWriter{banner: banner}
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dst != nil {
		return s.dst.Write(p)
	}
	return s.buf.Write(p)
}

// Promote flushes the backlog to dst and routes all further writes there.
// Only the first call takes effect.
func (s *switchWriter) Promote(dst io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dst != nil {
		return
	}
	if s.buf.Len() > 0 && s.banner != "" {
		run.Fprintf(dst, "%s", s.banner)
	}
	dst.Write(s.buf.Bytes())
	s.buf.Reset()
	s.dst = dst
}
