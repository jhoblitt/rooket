package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSwitchWriter(t *testing.T) {
	var dst bytes.Buffer
	sw := newSwitchWriter("=== banner ===\n")

	fmt.Fprint(sw, "buffered1\n")
	fmt.Fprint(sw, "buffered2\n")
	if dst.Len() != 0 {
		t.Fatal("wrote to destination before promotion")
	}

	sw.Promote(&dst)
	if got, want := dst.String(), "=== banner ===\nbuffered1\nbuffered2\n"; got != want {
		t.Fatalf("after promote: %q, want %q", got, want)
	}

	fmt.Fprint(sw, "live\n")
	if got := dst.String(); got != "=== banner ===\nbuffered1\nbuffered2\nlive\n" {
		t.Fatalf("live write missing: %q", got)
	}

	sw.Promote(&dst)
	if got := dst.String(); got != "=== banner ===\nbuffered1\nbuffered2\nlive\n" {
		t.Fatalf("second promote changed output: %q", got)
	}
}

func TestSwitchWriterNoBacklogNoBanner(t *testing.T) {
	var dst bytes.Buffer
	sw := newSwitchWriter("=== banner ===\n")
	sw.Promote(&dst)
	if dst.Len() != 0 {
		t.Fatalf("banner printed with empty backlog: %q", dst.String())
	}
	fmt.Fprint(sw, "live\n")
	if dst.String() != "live\n" {
		t.Fatalf("got %q", dst.String())
	}
}

func TestSwitchWriterConcurrent(t *testing.T) {
	const rounds, writers, lines = 50, 8, 100
	// A write that slipped into the backlog after Promote flushed it would
	// never be drained, a loss the race detector cannot see. That window is
	// narrow, so each run replays the race many times.
	for round := range rounds {
		counts := make(map[string]int, writers*lines)
		for line := range strings.Lines(promoteDuringWrites(writers, lines)) {
			counts[line]++
		}
		for w := range writers {
			for j := range lines {
				want := fmt.Sprintf("writer %d line %d\n", w, j)
				if counts[want] != 1 {
					t.Errorf("round %d: %q arrived %d times, want exactly once", round, want, counts[want])
				}
				delete(counts, want)
			}
		}
		for line := range counts {
			t.Errorf("round %d: unexpected output %q", round, line)
		}
		if t.Failed() {
			return
		}
	}
}

// promoteDuringWrites promotes a switchWriter while concurrent writers each
// write their own numbered lines to it, and returns what reached dst.
func promoteDuringWrites(writers, lines int) string {
	var mu sync.Mutex
	var dst bytes.Buffer
	syncDst := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return dst.Write(p)
	})
	sw := newSwitchWriter("")
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for j := range lines {
				fmt.Fprintf(sw, "writer %d line %d\n", w, j)
			}
		})
	}
	sw.Promote(syncDst)
	wg.Wait()
	return dst.String()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
