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
