package cmd

import (
	"os"
	"os/exec"
	"testing"
)

func TestRenderedSudoersPassesVisudo(t *testing.T) {
	if _, err := exec.LookPath("visudo"); err != nil {
		t.Skip("visudo not installed")
	}
	got, err := renderSudoers("tester", testPaths())
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}
	f := t.TempDir() + "/rooket"
	if err := os.WriteFile(f, []byte(got), 0o440); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("visudo", "-cf", f).CombinedOutput(); err != nil {
		t.Fatalf("visudo rejected the rendered rule: %v\n%s", err, out)
	}
}
