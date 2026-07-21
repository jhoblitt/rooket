package run

import (
	"bytes"
	"testing"
)

func TestEmitColorAndPlain(t *testing.T) {
	t.Cleanup(func() { SetColor(false); SetTimestamps(false) })
	SetTimestamps(false)

	SetColor(false)
	var buf bytes.Buffer
	Fprintf(&buf, "==> hi\n")
	if got := buf.String(); got != "==> hi\n" {
		t.Fatalf("plain status = %q, want unstyled", got)
	}

	SetColor(true)
	buf.Reset()
	Fprintf(&buf, "==> hi\n")
	if got, want := buf.String(), colorStatus+"==> hi"+colorReset+"\n"; got != want {
		t.Fatalf("colored status = %q, want %q", got, want)
	}

	buf.Reset()
	tracef(&buf, "make", []string{"-j"})
	if got, want := buf.String(), colorTrace+"+ make -j"+colorReset+"\n"; got != want {
		t.Fatalf("colored trace = %q, want %q", got, want)
	}

	// Each non-empty line is wrapped independently; blank lines stay blank.
	buf.Reset()
	Fprintf(&buf, "a\n\nb\n")
	want := colorStatus + "a" + colorReset + "\n\n" + colorStatus + "b" + colorReset + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("multiline colored = %q, want %q", got, want)
	}
}

func TestEmitColorWithTimestamp(t *testing.T) {
	t.Cleanup(func() { SetColor(false); SetTimestamps(false) })
	SetColor(true)
	SetTimestamps(true)

	var buf bytes.Buffer
	Fprintf(&buf, "go\n")
	got := buf.String()
	// Color wraps the whole line, timestamp prefix included.
	if len(got) < len(colorStatus)+len(colorReset) ||
		got[:len(colorStatus)] != colorStatus {
		t.Fatalf("expected color to wrap the timestamped line: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte("s] go")) {
		t.Fatalf("expected timestamp prefix inside the color: %q", got)
	}
}
