package cmd

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// clusterName resolves the cluster name in precedence order: the explicit
// --name flag value if non-empty, then $ROOKET_NAME, then the rook clone's
// absolute path encoded by encodePath (so two checkouts that share a basename
// in different directories get distinct clusters), then "rook".
func clusterName(flagName string) string {
	if flagName != "" {
		return flagName
	}
	if env := os.Getenv("ROOKET_NAME"); env != "" {
		return env
	}
	if wd, err := os.Getwd(); err == nil {
		if root := findRookRoot(wd); root != "" {
			return encodePath(root)
		}
	}
	return "rook"
}

// encodePath turns an absolute path into a unique, kind-safe cluster name by
// lowercasing it and collapsing each run of non-alphanumeric characters to a
// single dash (so /home/jhoblitt/github/rook3 -> home-jhoblitt-github-rook3).
// This mirrors the per-path directory scheme Claude Code uses, but drops the
// leading dash and lowercases: a kind cluster name — and the node and label
// names derived from it — must be lowercase and start with an alphanumeric. If
// the result would overrun a kind node name, it is truncated and a hash of the
// full path is appended to keep it unique.
func encodePath(p string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(p) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	const max = 45 // keep "<name>-control-plane" within the 63-char DNS label limit
	if len(s) > max {
		sum := sha256.Sum256([]byte(p))
		s = s[:max-9] + "-" + fmt.Sprintf("%x", sum[:4])
	}
	if s == "" {
		return "rook"
	}
	return s
}

// stateDirPath returns a cluster's state directory (~/.local/share/rooket/<name>)
// without creating it. The directory holds the cluster's disk images,
// kubeconfig, and metadata.
func stateDirPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "rooket", name), nil
}

// ensureStateDir returns a cluster's state directory, creating it.
func ensureStateDir(name string) (string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// kubeconfigPath returns a cluster's kubeconfig path (inside its state dir).
func kubeconfigPath(name string) (string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kubeconfig"), nil
}

// useCluster resolves the cluster name and points $KUBECONFIG at the cluster's
// own kubeconfig, so kind, kubectl, and helm all operate on that file instead
// of ~/.kube/config. It creates the state directory (so kind can write the
// kubeconfig there) and returns the resolved name.
func useCluster(flagName string) (string, error) {
	name := clusterName(flagName)
	if _, err := ensureStateDir(name); err != nil {
		return "", err
	}
	kc, err := kubeconfigPath(name)
	if err != nil {
		return "", err
	}
	if err := os.Setenv("KUBECONFIG", kc); err != nil {
		return "", err
	}
	return name, nil
}

// readRegistryPort returns the registry host port recorded for a cluster, or 0
// if none is recorded.
func readRegistryPort(name string) int {
	dir, err := stateDirPath(name)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(dir, "registry-port"))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return p
}

// writeRegistryPort records a cluster's chosen registry host port.
func writeRegistryPort(name string, port int) error {
	dir, err := ensureStateDir(name)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "registry-port"), []byte(strconv.Itoa(port)+"\n"), 0o644)
}

// freePort returns the first free TCP port on 127.0.0.1 at or above start.
func freePort(start int) (int, error) {
	for p := start; p < start+1000; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free TCP port in [%d, %d)", start, start+1000)
}

// resolveRegistryPort decides a cluster's registry host port: the port already
// recorded for the cluster (so repeat runs reuse it), else the explicit
// --registry-port value if the user set it, else a free port at or above 5001.
func resolveRegistryPort(name string, flagPort int, flagChanged bool) (int, error) {
	if p := readRegistryPort(name); p != 0 {
		return p, nil
	}
	if flagChanged {
		return flagPort, nil
	}
	return freePort(5001)
}
