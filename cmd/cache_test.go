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
