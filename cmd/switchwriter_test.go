package cmd

import (
	"bytes"
	"fmt"
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
	var mu sync.Mutex
	var dst bytes.Buffer
	syncDst := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return dst.Write(p)
	})
	sw := newSwitchWriter("")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				fmt.Fprintf(sw, "line %d\n", j)
			}
		}()
	}
	sw.Promote(syncDst)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if dst.Len() == 0 {
		t.Fatal("no output after concurrent writes")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
