package cmd

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/engine"
)

// clusterName resolves the name of the cluster a command with a --name flag
// acts on; see resolveClusterName.
func clusterName(flagName string) (string, error) {
	return resolveClusterName(flagName, "", noClusterError{takesName: true})
}

// clusterNameOrDir is clusterName for a command that also takes --dir, a rook
// clone to work from.
func clusterNameOrDir(flagName, dir string) (string, error) {
	return resolveClusterName(flagName, dir, noClusterError{takesName: true, dirWays: "--dir"})
}

// clusterNameOrRookDir is clusterNameOrDir for a command that finds its rook
// tree with resolveRookDir, which takes $ROOK_DIR in place of an unset --dir.
// released says the command was given --rook-version: it then reads no tree,
// only released charts, and --dir merely locates its configuration home, so
// neither --dir nor $ROOK_DIR names the cluster.
func clusterNameOrRookDir(flagName, dir string, released bool) (string, error) {
	if released {
		return clusterName(flagName)
	}
	return resolveClusterName(flagName, namedRookDir(dir), noClusterError{takesName: true, dirWays: rookDirWays})
}

// envClusterName is clusterName for a command with no --name flag.
func envClusterName() (string, error) {
	return resolveClusterName("", "", noClusterError{})
}

// rookDirClusterName is clusterNameOrRookDir for a command with no --name flag.
func rookDirClusterName(dir string, released bool) (string, error) {
	if released {
		return envClusterName()
	}
	return resolveClusterName("", namedRookDir(dir), noClusterError{dirWays: rookDirWays})
}

// rookDirWays is how a command reading its tree with resolveRookDir can be
// pointed at a clone.
const rookDirWays = "--dir or $ROOK_DIR"

// resolveClusterName resolves a cluster name in precedence order: the --name
// flag value if non-empty, then $ROOKET_NAME, then the rook clone enclosing the
// working directory, then the one enclosing dir, the clone the command was
// pointed at. A clone is named by its root's absolute path encoded by
// encodePath, so two checkouts that share a basename in different directories
// get distinct clusters, and a command pointed at a clone gets the name it
// would get inside that clone. The working directory's clone outranks dir's
// so that no name a clone already gave a cluster changes.
//
// With none of them it returns refusal rather than guess: a fixed default
// would have the command act on whatever cluster holds that name, possibly
// someone else's, which down would destroy.
func resolveClusterName(flagName, dir string, refusal noClusterError) (string, error) {
	if flagName != "" {
		return flagName, nil
	}
	if env := os.Getenv("ROOKET_NAME"); env != "" {
		return env, nil
	}
	if wd, err := os.Getwd(); err == nil {
		if root := findRookRoot(wd); root != "" {
			return encodePath(root), nil
		}
	}
	if root := enclosingClone(dir); root != "" {
		return encodePath(root), nil
	}
	return "", refusal
}

// noClusterError refuses a command that names no cluster, offering only what
// the command accepts: takesName says whether it has a --name flag, and
// dirWays names what points it at a clone, or is empty when nothing does.
type noClusterError struct {
	takesName bool
	dirWays   string
}

func (e noClusterError) Error() string {
	ways := []string{"set $ROOKET_NAME", "run inside a rook clone"}
	legacy := "ROOKET_NAME=rook"
	if e.takesName {
		ways = append([]string{"pass --name"}, ways...)
		legacy = "--name rook"
	}
	if e.dirWays != "" {
		ways = append(ways, "point "+e.dirWays+" at one")
	}
	last := len(ways) - 1
	list := strings.Join(ways[:last], ", ")
	if last > 1 {
		list += ","
	}
	return fmt.Sprintf(`no cluster selected: %s or %s (a cluster made by an older rooket is named "rook": select it with %s)`,
		list, ways[last], legacy)
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

// stateDirRoot returns the directory that holds every cluster's state dir
// (~/.local/share/rooket).
func stateDirRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "rooket"), nil
}

// stateDirNames returns the state root and the cluster names that have a state
// directory under it. A root that does not exist yet is an empty list, not an
// error.
func stateDirNames() (string, []string, error) {
	root, err := stateDirRoot()
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return root, nil, nil
		}
		return "", nil, err
	}
	var names []string
	for _, e := range entries {
		// A dot-led directory is rooket's own host-wide state, such as the
		// build cache; no cluster name, being a DNS label, can produce one.
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return root, names, nil
}

// clusterNameRE matches a single lowercase RFC-1123 DNS label: what kind
// accepts as a cluster name and what is safe to join onto the state-dir root as
// one path segment. It admits no '/', '.', or '..', so a name cannot traverse
// out of the root.
var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validateClusterName rejects names that are not a single lowercase DNS label.
// Enforced centrally in stateDirPath so that an explicit --name or $ROOKET_NAME
// cannot escape the state-dir root before rooket truncates images or removes
// directories under it. encodePath already produces conforming names.
func validateClusterName(name string) error {
	if len(name) > 63 {
		return fmt.Errorf("invalid cluster name %q: longer than 63 characters", name)
	}
	if !clusterNameRE.MatchString(name) {
		return fmt.Errorf("invalid cluster name %q: must be a lowercase DNS label (letters, digits, and internal dashes only)", name)
	}
	return nil
}

// stateDirPath returns a cluster's state directory (~/.local/share/rooket/<name>)
// without creating it. The directory holds the cluster's disk images,
// kubeconfig, and metadata. The name is validated first so it stays a single
// path segment within the state root.
func stateDirPath(name string) (string, error) {
	if err := validateClusterName(name); err != nil {
		return "", err
	}
	root, err := stateDirRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// ensureStateDir returns a cluster's state directory, creating it and
// recording the rook clone it belongs to: the one enclosing the working
// directory, unless a command pointed at another clone recorded that first
// (see ensureStateDirFrom). Recording here rather than in the build stamp is
// what keeps prune's parked-vs-abandoned test (see clonePathFile) working for
// a cluster brought up without ever building rook.
func ensureStateDir(name string) (string, error) {
	return ensureStateDirFrom(name, enclosingClone("."))
}

// ensureStateDirFrom is ensureStateDir for a cluster built from clone. The
// first clone recorded stands, so a command pointed at a clone other than the
// working directory's calls this before anything else writes the state dir.
func ensureStateDirFrom(name, clone string) (string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	recordClonePath(dir, clone)
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

// requireKubeconfig returns a cluster's kubeconfig path, or an error asking
// whether the cluster is up when no kubeconfig is there. Commands that need
// the cluster call it first, so a cluster that is down reads as that rather
// than as "connection refused" noise from kubectl or helm chasing a missing
// kubeconfig.
func requireKubeconfig(name string) (string, error) {
	kc, err := kubeconfigPath(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(kc); err != nil {
		return "", fmt.Errorf("no kubeconfig for cluster %q at %s (is it up?)", name, kc)
	}
	return kc, nil
}

// helmEnv returns environment variables pointing helm at a per-cluster,
// per-purpose config/cache/data triplet inside the cluster's state dir,
// creating the directories. Each cluster gets its own helm world — repos
// added via 'rooket helm' belong to that cluster only, and rooket never
// touches (or is slowed by) the host's helm configuration. The "make"
// purpose is kept separate from "rooket" so rook's `helm dependency` runs
// inside make can never contend on helm's non-atomic config and cache files
// with rooket's own helm invocations.
//
// Helm honors path-specific variables (HELM_REPOSITORY_CONFIG etc.)
// independently of the three homes, so those are pinned explicitly too —
// otherwise a value exported in the parent environment would silently
// bypass the triplet. Helm is a Go program, and Go children resolve
// duplicate environment entries last-wins, so appending these to the
// inherited environment overrides any ambient values.
func helmEnv(name, purpose string) ([]string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return nil, err
	}
	return helmEnvAt(filepath.Join(dir, "helm", purpose))
}

// helmEnvAt returns environment variables pointing helm at a config/cache/data
// triplet under base, creating the directories; see helmEnv.
func helmEnvAt(base string) ([]string, error) {
	homes := map[string]string{}
	env := make([]string, 0, 7)
	for _, sub := range []struct{ envVar, subdir string }{
		{"HELM_CONFIG_HOME", "config"},
		{"HELM_CACHE_HOME", "cache"},
		{"HELM_DATA_HOME", "data"},
	} {
		p := filepath.Join(base, sub.subdir)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, fmt.Errorf("create helm %s dir: %w", sub.subdir, err)
		}
		homes[sub.subdir] = p
		env = append(env, sub.envVar+"="+p)
	}
	env = append(env,
		"HELM_REPOSITORY_CONFIG="+filepath.Join(homes["config"], "repositories.yaml"),
		"HELM_REPOSITORY_CACHE="+filepath.Join(homes["cache"], "repository"),
		"HELM_REGISTRY_CONFIG="+filepath.Join(homes["config"], "registry", "config.json"),
		"HELM_PLUGINS="+filepath.Join(homes["data"], "plugins"),
	)
	return env, nil
}

// useCluster resolves the cluster name and points $KUBECONFIG at the cluster's
// own kubeconfig, so kind, kubectl, and helm all operate on that file instead
// of ~/.kube/config. It does not create the state directory — commands that
// write state (create/up via writeRegistryPort, block setup) create it, so
// read-only commands like delete don't leave empty dirs behind.
func useCluster(flagName string) (string, error) {
	return pointKubeconfig(clusterName(flagName))
}

// useClusterOrDir is useCluster for a command that also takes --dir. released
// says the command was given --rook-version, when --dir does not name the
// cluster; see clusterNameOrRookDir.
func useClusterOrDir(flagName, dir string, released bool) (string, error) {
	if released {
		return useCluster(flagName)
	}
	return pointKubeconfig(clusterNameOrDir(flagName, dir))
}

// useClusterOrRookDir is useCluster for a command that finds its rook tree
// with resolveRookDir; see clusterNameOrRookDir.
func useClusterOrRookDir(flagName, dir string, released bool) (string, error) {
	return pointKubeconfig(clusterNameOrRookDir(flagName, dir, released))
}

// pointKubeconfig points $KUBECONFIG at the kubeconfig of the cluster a
// resolver named, or passes on the resolver's refusal.
func pointKubeconfig(name string, err error) (string, error) {
	if err != nil {
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

// recordedPorts maps each registry port another cluster has recorded to the
// cluster that recorded it, skipping the caller's own.
//
// A bind probe cannot see these. A cluster whose registry is merely stopped —
// the normal state after a reboot — holds no port at all, so probing alone
// hands its port to whoever asks next; that cluster's next run then finds its
// port taken, discards its registry and re-picks, losing the images it had
// pushed. Ports are cheap and that churn is not.
func recordedPorts(exceptCluster string) map[int]string {
	_, names, err := stateDirNames()
	if err != nil {
		return nil
	}
	taken := map[int]string{}
	for _, n := range names {
		if n == exceptCluster {
			continue
		}
		if p := readRegistryPort(n); p != 0 {
			taken[p] = n
		}
	}
	return taken
}

// freePortFor returns the first TCP port on 127.0.0.1 at or above start that is
// bindable and not spoken for by another cluster.
//
// Probing remains a guess about everything that is not rooket: a foreign
// process can still take the port between this check and the registry binding
// it. That window is tiny, and a loser gets a clear bind error plus the
// re-pick on its next run, so it is left alone. What is not left alone is
// rooket colliding with itself, which the recorded-port set and the allocation
// lock together remove.
func freePortFor(cluster string, start int) (int, error) {
	taken := recordedPorts(cluster)
	for p := start; p < start+1000; p++ {
		if _, ok := taken[p]; ok {
			continue
		}
		if portProbe(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free TCP port in [%d, %d) that is not already assigned to another cluster",
		start, start+1000)
}

// portProbe answers whether a host port is bindable. Indirected so tests can
// state the machine's port availability rather than inherit whatever happens to
// be listening on the developer's workstation — including, routinely, a rooket
// registry on 5001.
var portProbe = portFree

// portFree reports whether TCP port p on 127.0.0.1 can be bound.
func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// resolveRegistryPort decides a cluster's registry host port. An explicit
// --registry-port wins, except that conflicting with the port already recorded
// for the cluster is an error rather than a silent override: the registry
// container and every node's containerd mirror config were set up with the
// recorded port, so changing it requires recreating the cluster. Without the
// flag, the recorded port is reused (so repeat runs stay consistent), else the
// first free port from 5001.
func resolveRegistryPort(name string, flagPort int, flagChanged bool) (int, error) {
	persisted := readRegistryPort(name)
	if flagChanged {
		if persisted != 0 && persisted != flagPort {
			return 0, fmt.Errorf("cluster %q already uses registry port %d; "+
				"omit --registry-port to reuse it, or run 'rooket down' first to change it",
				name, persisted)
		}
		return flagPort, nil
	}
	if persisted != 0 {
		return persisted, nil
	}
	return freePortFor(name, 5001)
}

// liveClusters returns the union of kind cluster names across every installed
// container engine, mapping each name to the engines that report it. consulted
// lists the engines successfully queried; failed lists engines that are
// installed but could not be queried (e.g. a stopped daemon). Querying all
// engines rather than the session's resolved one keeps prune and list from
// misclassifying another engine's live cluster as gone.
func liveClusters() (live map[string][]engine.Engine, consulted, failed []engine.Engine) {
	live = map[string][]engine.Engine{}
	for _, eng := range []engine.Engine{engine.Podman, engine.Docker} {
		if _, err := exec.LookPath(eng.String()); err != nil {
			continue
		}
		// The trace goes to stderr so it cannot land inside list's table on
		// stdout, which is the only machine-readable thing rooket prints.
		names, err := cluster.List(os.Stderr, eng)
		if err != nil {
			failed = append(failed, eng)
			continue
		}
		consulted = append(consulted, eng)
		for _, n := range names {
			live[n] = append(live[n], eng)
		}
	}
	return live, consulted, failed
}
