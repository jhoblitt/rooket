package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
)

func TestEditValuesSeedsWhenAbsent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	seed := []byte("# rooket base\n# toolbox:\n#   enabled: true\n")

	var sawContent string
	err := editValues(p, seed, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sawContent = string(data)
		return os.WriteFile(path, []byte("toolbox:\n  enabled: false\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawContent, "# toolbox:") {
		t.Errorf("editor did not see the seed: %q", sawContent)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "enabled: false") {
		t.Errorf("saved file = %q", got)
	}
}

func TestEditValuesRemovesEmptyResult(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := editValues(p, nil, func(path string) error {
		return os.WriteFile(path, []byte("# everything commented out\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty result should remove the layer file")
	}
}

func TestEditValuesReopensOnParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	calls := 0
	err := editValues(p, nil, func(path string) error {
		calls++
		if calls == 1 {
			return os.WriteFile(path, []byte("a: [1,\n"), 0o644)
		}
		return os.WriteFile(path, []byte("a: 1\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("editor called %d times, want 2", calls)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Errorf("saved %q", data)
	}
}

// TestWriteFileAtomicLeavesOldFileOnFailure induces a real write failure (a
// read-only directory blocks the sibling temp file's creation) and checks
// that the pre-existing file survives untouched, rather than being truncated
// by an in-place write. Skipped under root, which bypasses directory
// permission bits and so can't be used to induce the failure.
func TestWriteFileAtomicLeavesOldFileOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission check this test relies on to induce a write failure")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(p, []byte("old: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if err := writeFileAtomic(p, []byte("new: true\n")); err == nil {
		t.Fatal("expected writeFileAtomic to fail against a read-only directory")
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old: true\n" {
		t.Errorf("pre-existing file was modified by the failed write: %q", got)
	}
}

// TestEditValuesLeavesNoStrayTempFiles is the positive-invariant companion to
// TestWriteFileAtomicLeavesOldFileOnFailure: on the successful path, the
// sibling temp file used for the atomic rename must not linger next to the
// target.
func TestEditValuesLeavesNoStrayTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "values.yaml")

	err := editValues(p, nil, func(path string) error {
		return os.WriteFile(path, []byte("a: 1\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "values.yaml" {
		t.Errorf("directory contents after a successful edit = %v, want only values.yaml", entries)
	}
}

// TestEditValuesReopenMessageNamesTargetFile asserts the reopen-on-parse-error
// message names the file the user believes they're editing rather than the
// ephemeral temp file, while still keeping the underlying yaml error's detail
// (e.g. line/column).
func TestEditValuesReopenMessageNamesTargetFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	calls := 0
	editErr := editValues(p, nil, func(path string) error {
		calls++
		if calls == 1 {
			return os.WriteFile(path, []byte("a: [1,\n"), 0o644)
		}
		return os.WriteFile(path, []byte("a: 1\n"), 0o644)
	})

	w.Close()
	os.Stdout = oldStdout
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := r.Read(buf)
		out.Write(buf[:n])
		if rerr != nil {
			break
		}
	}

	if editErr != nil {
		t.Fatal(editErr)
	}
	printed := out.String()
	if !strings.Contains(printed, p) {
		t.Errorf("reopen message = %q, want it to name the target file %q", printed, p)
	}
	if strings.Contains(printed, "rooket-values-") {
		t.Errorf("reopen message = %q, leaked the ephemeral temp file's name", printed)
	}
	if !strings.Contains(printed, "line 1") {
		t.Errorf("reopen message = %q, lost the underlying yaml parse detail", printed)
	}
}

// TestValuesEditMultiChartFailureNamesChartAndNotesEarlierSaves drives the
// real "rooket values edit" command tree (no chart argument, so all three
// charts are edited in sequence) with a scripted $EDITOR that succeeds for
// the operator chart and fails for the cluster chart. The resulting error
// must name the chart that failed and make clear that the operator chart's
// edit, which runs first, was already committed.
func TestValuesEditMultiChartFailureNamesChartAndNotesEarlierSaves(t *testing.T) {
	dir := t.TempDir()

	script := filepath.Join(t.TempDir(), "fake-editor.sh")
	scriptBody := "#!/bin/sh\n" +
		"f=\"$1\"\n" +
		"if grep -q '^# rook-ceph overrides' \"$f\"; then\n" +
		"	printf 'operator: true\\n' > \"$f\"\n" +
		"	exit 0\n" +
		"elif grep -q '^# rook-ceph-cluster overrides' \"$f\"; then\n" +
		"	exit 1\n" +
		"fi\n" +
		"printf 'csi: true\\n' > \"$f\"\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)

	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	})
	rootCmd.SetArgs([]string{"values", "edit", "--dir", dir})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error from the cluster chart's failing editor")
	}
	if !strings.Contains(err.Error(), chartCluster) {
		t.Errorf("error %q does not name the failing chart %q", err.Error(), chartCluster)
	}
	if !strings.Contains(err.Error(), "already saved") {
		t.Errorf("error %q does not note that earlier charts in the run were already saved", err.Error())
	}

	cloneDir := clone.Open(dir)
	if _, statErr := os.Stat(cloneDir.ValuesPath(chartOperator)); statErr != nil {
		t.Errorf("expected the operator chart's edit to have already been committed before the cluster chart failed: %v", statErr)
	}
}
