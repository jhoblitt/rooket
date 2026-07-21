package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/jhoblitt/rooket/internal/values"
)

var valuesEditCmd = &cobra.Command{
	Use:   "edit [chart]",
	Short: "Edit this clone's values overrides in $EDITOR",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		charts := []string{chartOperator, chartCluster, chartCSI}
		if len(args) == 1 {
			c, err := chartName(args[0])
			if err != nil {
				return err
			}
			charts = []string{c}
		}

		cloneDir := clone.Open(dir)
		if err := cloneDir.Ensure(); err != nil {
			return err
		}
		for _, chart := range charts {
			seed, err := seedFor(chart)
			if err != nil {
				return err
			}
			if err := editValues(cloneDir.ValuesPath(chart), seed, launchEditor); err != nil {
				return err
			}
		}
		return nil
	},
}

// seedFor renders rooket's generated layer as commented YAML. Knowing which of
// the chart's keys exist and what rooket already set is the hard part of
// overriding one, so a new file starts as the answer to both.
func seedFor(chart string) ([]byte, error) {
	data, err := values.Encode(showBase(chart))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s overrides for this clone.\n", chart)
	b.WriteString("# Uncomment and edit to override; delete everything to drop this layer.\n")
	b.WriteString("# Below is rooket's generated base — your values merge on top of it.\n#\n")
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		fmt.Fprintf(&b, "# %s\n", line)
	}
	return []byte(b.String()), nil
}

// editValues opens path in the editor, reopening on a parse error rather than
// saving a broken layer that would fail later inside helm upgrade. A result
// with no keys removes the file instead of leaving an empty layer.
func editValues(path string, seed []byte, edit func(string) error) error {
	tmp, err := os.CreateTemp("", "rooket-values-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		_, err = tmp.Write(existing)
	case os.IsNotExist(err):
		_, err = tmp.Write(seed)
	}
	if err != nil {
		return fmt.Errorf("seed temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	for {
		if err := edit(tmp.Name()); err != nil {
			return err
		}
		m, err := values.LoadFile(tmp.Name())
		if err == nil {
			if len(m) == 0 {
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					return fmt.Errorf("remove %s: %w", path, rmErr)
				}
				run.Printf("==> %s left empty; layer removed\n", filepath.Base(path))
				return nil
			}
			data, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			run.Printf("==> wrote %s\n", path)
			return nil
		}
		run.Printf("==> %v\n==> reopening the editor\n", err)
	}
}

func launchEditor(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	// $EDITOR routinely carries arguments ("code --wait", "emacsclient -nw"), so
	// it has to go through a shell rather than exec.Command(ed, path). The path
	// is passed as the positional $1 instead of being interpolated into the
	// command string, so it cannot inject; the only code that runs is whatever
	// the user already put in $EDITOR.
	c := exec.Command("sh", "-c", ed+" \"$1\"", "sh", path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func init() {
	valuesCmd.AddCommand(valuesEditCmd)
}
