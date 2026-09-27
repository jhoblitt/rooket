package cmd

import (
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestRenderProfileList(t *testing.T) {
	got := renderProfileList([]profiles.Profile{
		{Name: "rbd", Description: "block storage", BuiltIn: true},
		{Name: "mine", Description: "my thing", BuiltIn: false},
	}, []string{"rbd"})

	if !strings.Contains(got, "built-in") {
		t.Errorf("missing built-in marker:\n%s", got)
	}
	if !strings.Contains(got, "user") {
		t.Errorf("missing user marker:\n%s", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	var rbdLine string
	for _, l := range lines {
		if strings.Contains(l, "rbd") {
			rbdLine = l
		}
	}
	if !strings.Contains(rbdLine, "*") {
		t.Errorf("active profile not marked: %q", rbdLine)
	}
}

func TestRenderProfileListShowsActivePathProfile(t *testing.T) {
	got := renderProfileList([]profiles.Profile{
		{Name: "rbd", Description: "block storage", BuiltIn: true},
		{Name: "mytest", Path: "./mytest", Description: "my test"},
	}, []string{"./mytest"})

	var pathLine, rbdLine string
	for l := range strings.Lines(got) {
		switch {
		case strings.Contains(l, "./mytest"):
			pathLine = l
		case strings.Contains(l, "rbd"):
			rbdLine = l
		}
	}
	if !strings.Contains(pathLine, "*") || !strings.Contains(pathLine, "path") {
		t.Errorf("path profile line = %q, want it marked active with origin path", pathLine)
	}
	if strings.Contains(rbdLine, "*") {
		t.Errorf("rbd line = %q, want it unmarked", rbdLine)
	}
}

func TestListedProfilesAddsActivePaths(t *testing.T) {
	path := writePathProfile(t, t.TempDir(), "mytest", nil)

	got, err := listedProfiles(t.TempDir(), []string{"rbd", path})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range got {
		if p.Path != "" {
			paths = append(paths, p.Path)
		}
	}
	if len(paths) != 1 || paths[0] != path {
		t.Errorf("path profiles listed = %#v, want exactly [%s]", paths, path)
	}
}
