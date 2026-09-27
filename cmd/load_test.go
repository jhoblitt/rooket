package cmd

import (
	"strings"
	"testing"
)

// load's help shows what each example image is pushed as, which a reader
// copies into a manifest; it must be the name load pushes.
func TestLoadHelpExamplesAreWhatLoadPushes(t *testing.T) {
	lines := strings.Split(loadCmd.Long, "\n")
	examples := 0
	for i, line := range lines {
		src, ok := strings.CutPrefix(strings.TrimSpace(line), "rooket load ")
		if !ok {
			continue
		}
		var pushed string
		if i+1 < len(lines) {
			pushed, ok = strings.CutPrefix(strings.TrimSpace(lines[i+1]), "# pushes as ")
		}
		if !ok {
			t.Errorf("example %q is not followed by what it pushes as", src)
			continue
		}
		examples++
		if want := "localhost:5001/" + imageBasename(src); pushed != want {
			t.Errorf("help says %s pushes as %s; load pushes it as %s", src, pushed, want)
		}
	}
	if examples == 0 {
		t.Fatal("found no examples in load's help")
	}
}
