package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/cache"
)

func TestCacheSummaryReady(t *testing.T) {
	got := cacheSummary(true, nil)
	if !strings.Contains(got, cache.ContainerName) {
		t.Errorf("ready summary should name the container, got %q", got)
	}
	if strings.Contains(got, "UNAVAILABLE") {
		t.Errorf("ready summary must not read as a failure, got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("ready summary must stay on the banner's one line, got %q", got)
	}
}

func TestCacheSummaryUnavailable(t *testing.T) {
	cause := errors.New("Requesting bearer token: received unexpected HTTP status: 403 Forbidden")
	got := cacheSummary(false, cause)

	// The cause is the whole point: a bare "unavailable" sends the reader back
	// to scrollback they have already lost.
	for _, want := range []string{
		"UNAVAILABLE",
		cause.Error(),
		"NOT wired",
		"re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failure summary missing %q:\n%s", want, got)
		}
	}
}

// TestCacheSummaryAlignment pins the continuation indent to the banner's value
// column. The label and the indent are declared in different files, so a later
// edit to either silently ragged-edges the block without this.
func TestCacheSummaryAlignment(t *testing.T) {
	const label = "  image cache:       " // as written in createClusterRun's banner
	got := cacheSummary(false, errors.New("boom"))

	for _, line := range strings.Split(got, "\n")[1:] {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent != len(label) {
			t.Errorf("continuation indent %d != label width %d; banner would be ragged:\n%s",
				indent, len(label), got)
		}
	}
}
