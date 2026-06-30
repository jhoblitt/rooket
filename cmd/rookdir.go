package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// rookModulePath is the Go module path declared by the rook source repository.
const rookModulePath = "github.com/rook/rook"

// resolveRookDir determines the rook source directory, in order of precedence:
// the explicit --dir flag, the ROOK_DIR environment variable, then walking up
// from the current directory for a go.mod that declares the rook module.
func resolveRookDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if env := os.Getenv("ROOK_DIR"); env != "" {
		return env, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine working directory: %w", err)
	}
	if root := findRookRoot(wd); root != "" {
		fmt.Printf("==> detected rook source at %s\n", root)
		return root, nil
	}
	return "", fmt.Errorf("could not locate a rook source tree: pass --dir, set ROOK_DIR, "+
		"or run from within a rook clone (no go.mod declaring module %q in %s or any parent directory)",
		rookModulePath, wd)
}

// findRookRoot walks up from dir (inclusive) and returns the first directory
// holding a go.mod that declares the rook module, or "" if it reaches the
// filesystem root without finding one.
func findRookRoot(dir string) string {
	for {
		if isRookModuleDir(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isRookModuleDir reports whether dir contains a go.mod declaring the rook module.
func isRookModuleDir(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return modfile.ModulePath(data) == rookModulePath
}
