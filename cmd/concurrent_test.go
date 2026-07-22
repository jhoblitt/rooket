package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

// runConcurrent must flush each branch's output in the order the funcs were
// passed, regardless of the order they finish, and it must actually run them
// concurrently. The barrier proves concurrency: every branch blocks until all
// have started, so a serial implementation would deadlock (caught as a test
// timeout).
func TestRunConcurrentOrdersOutputAndRunsConcurrently(t *testing.T) {
	const n = 5
	var barrier sync.WaitGroup
	barrier.Add(n)
	fns := make([]func(io.Writer) error, n)
	for i := 0; i < n; i++ {
		fns[i] = func(w io.Writer) error {
			barrier.Done()
			barrier.Wait()
			fmt.Fprintf(w, "%d\n", i)
			return nil
		}
	}
	var out bytes.Buffer
	if err := runConcurrent(&out, fns...); err != nil {
		t.Fatalf("runConcurrent: %v", err)
	}
	if got, want := out.String(), "0\n1\n2\n3\n4\n"; got != want {
		t.Errorf("output = %q, want %q (buffers must flush in func order)", got, want)
	}
}

// A failing branch must not suppress its siblings' output or errors: every
// branch's buffer is still flushed in order, and every error is joined.
func TestRunConcurrentFlushesAllAndJoinsErrors(t *testing.T) {
	errA := errors.New("boom-a")
	errC := errors.New("boom-c")
	var out bytes.Buffer
	err := runConcurrent(&out,
		func(w io.Writer) error { fmt.Fprint(w, "a"); return errA },
		func(w io.Writer) error { fmt.Fprint(w, "b"); return nil },
		func(w io.Writer) error { fmt.Fprint(w, "c"); return errC },
	)
	if !errors.Is(err, errA) || !errors.Is(err, errC) {
		t.Errorf("joined error = %v, want both %v and %v", err, errA, errC)
	}
	if got, want := out.String(), "abc"; got != want {
		t.Errorf("output = %q, want %q (every branch flushes even when one fails)", got, want)
	}
}

func TestRunConcurrentEmpty(t *testing.T) {
	if err := runConcurrent(io.Discard); err != nil {
		t.Errorf("runConcurrent() with no funcs = %v, want nil", err)
	}
}
