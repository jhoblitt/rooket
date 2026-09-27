package run

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestOutputContextToReturnsTrimmedStdout(t *testing.T) {
	var buf bytes.Buffer
	out, err := OutputContextTo(context.Background(), &buf, "echo", "hi")
	if err != nil || out != "hi" {
		t.Fatalf("OutputContextTo() = %q, %v; want \"hi\"", out, err)
	}
	if got, want := buf.String(), "+ echo hi\n"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// A command still running when its context ends is killed, and the error
// says the context ended, so the caller can report the deadline rather than
// a bare "signal: killed".
func TestOutputContextToKillsACommandThatOutlivesItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := OutputContextTo(ctx, io.Discard, "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OutputContextTo() error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want promptly after the 100ms deadline", d)
	}
}

// Killing the command leaves its own children running, and one that holds
// the command's output open would otherwise keep the read waiting as long as
// it lives.
func TestOutputContextToDoesNotWaitOnAChildHoldingItsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := OutputContextTo(ctx, io.Discard, "sh", "-c", "sleep 8 & wait")
	if err == nil {
		t.Fatal("OutputContextTo() succeeded for a command killed by its deadline")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want well before the 8s child exits", d)
	}
}
