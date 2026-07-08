package cmd

import (
	"strings"
	"testing"
)

func TestValidateClusterName(t *testing.T) {
	valid := []string{"rook", "rook3", "home-jhoblitt-github-rook3", "a", "a1", "x-y-z"}
	for _, n := range valid {
		if err := validateClusterName(n); err != nil {
			t.Errorf("validateClusterName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{
		"",                      // empty
		"../etc",                // traversal
		"..",                    // parent
		"a/b",                   // path separator
		"Rook",                  // uppercase
		"foo.bar",               // dot
		"-lead",                 // leading dash
		"trail-",                // trailing dash
		"has space",             // space
		"semi;colon",            // shell metacharacter
		strings.Repeat("a", 64), // longer than a DNS label
	}
	for _, n := range invalid {
		if err := validateClusterName(n); err == nil {
			t.Errorf("validateClusterName(%q) = nil, want error", n)
		}
	}
}

func TestStateDirPathRejectsTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, n := range []string{"../escape", "a/b", "..", "with space"} {
		if _, err := stateDirPath(n); err == nil {
			t.Errorf("stateDirPath(%q) = nil error, want rejection", n)
		}
	}
	if _, err := stateDirPath("valid-name"); err != nil {
		t.Errorf("stateDirPath(valid-name) = %v, want nil", err)
	}
}

// encodePath must only ever produce names that pass validateClusterName, so the
// auto-derived path never trips the guard that stateDirPath now enforces.
func TestEncodePathProducesValidNames(t *testing.T) {
	paths := []string{
		"/home/jhoblitt/github/rook3",
		"/Home/A.B/Rook",
		"/a//b/",
		"/",
		"/home/jhoblitt/" + strings.Repeat("verydeep/", 12) + "rook",
		"/123/456",
		"/!!!weird***path",
		"/trailing-dash-/",
	}
	for _, p := range paths {
		n := encodePath(p)
		if err := validateClusterName(n); err != nil {
			t.Errorf("encodePath(%q) = %q, which fails validation: %v", p, n, err)
		}
	}
}
