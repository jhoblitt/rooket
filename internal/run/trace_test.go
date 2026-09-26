package run

import (
	"bytes"
	"testing"
)

// Two runs of the same command that differ only in their environment — the
// kind provider being the case that matters — must not echo as the same line
// twice, or the trace reads as a duplicated command.
func TestOutputWithEnvEchoedToRendersEnvInTrace(t *testing.T) {
	var buf bytes.Buffer
	if _, err := OutputWithEnvEchoedTo(&buf, []string{"KIND_EXPERIMENTAL_PROVIDER=podman"}, "true"); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "+ KIND_EXPERIMENTAL_PROVIDER=podman true\n"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// The plain form stays quiet about the environment: its callers pass helm's
// whole config/cache/data triplet, which would bury the command it echoes.
func TestOutputWithEnvToOmitsEnvFromTrace(t *testing.T) {
	var buf bytes.Buffer
	if _, err := OutputWithEnvTo(&buf, []string{"HELM_CONFIG_HOME=/x", "HELM_CACHE_HOME=/y"}, "true"); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "+ true\n"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}
