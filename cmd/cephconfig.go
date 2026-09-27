package cmd

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	cephConfigName string
	cephConfigOut  string
)

var cephConfigCmd = &cobra.Command{
	Use:   "ceph-config",
	Short: "Write a ceph.conf and admin keyring for a librados client on the host",
	Long: `ceph-config writes what a librados client running on the host needs to reach
the cluster's Ceph into the --out directory, creating it if missing:

  ceph.conf                   mon_host listing every mon's v2 address, and a
                              [client.admin] section naming the keyring
  ceph.client.admin.keyring   the client.admin keyring (mode 0600)

so <out>/ceph.conf is the only path the client has to be given. Both come from
the Ceph toolbox the cluster chart deploys. The cluster is selected the same
way as the rest of rooket: --name, else $ROOKET_NAME, else the name derived
from the enclosing rook clone's path. ceph.conf values are unquoted, so --out
may not contain # ; $ \ or a line break.

The host can reach the mons only when they listen on the node's network, so
ceph-config refuses a cluster that is not host-networked. Rook does not
support changing a running cluster's network, so recreate such a cluster with
the host-network profile:

  rooket down
  rooket up --with host-network

and add host-network to the profiles list in the configuration home's
config.yaml (a clone's .rooket, or --config-dir) from that first up on, so a
later up keeps it rather than moving the running cluster off host networking.

ceph.conf records whether the admin key is AES, and ceph-config warns when it
is not: an AES256KRB5 key, for one, cannot be parsed by librados older than
19.2.6 / 20.2.4.

  rooket ceph-config --out ./ceph
  rooket ceph-config --name mycluster --out /tmp/ceph
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, _, err := outPaths(cephConfigOut); err != nil {
			return err
		}
		name, err := useCluster(cephConfigName)
		if err != nil {
			return err
		}
		kc, err := kubeconfigPath(name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(kc); err != nil {
			return fmt.Errorf("no kubeconfig for cluster %q at %s (is it up?)", name, kc)
		}
		conf, err := exportCephConfig(cephConfigOut, os.Stderr)
		if err != nil {
			return err
		}
		run.Printf("==> wrote %s\n", conf)
		return nil
	},
}

// kubectlOutput runs kubectl for ceph-config. Indirected so tests can answer
// with canned output instead of a cluster.
var kubectlOutput = runKubectl

// runKubectl runs kubectl against the cluster $KUBECONFIG names (see
// useCluster) and returns its trimmed stdout. A failure carries kubectl's
// stderr, without which it would say only "exit status 1" — not, for one,
// that the toolbox is missing.
func runKubectl(args ...string) (string, error) {
	out, err := run.Output("kubectl", args...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// toolboxArgs returns the kubectl arguments that run command in the Ceph
// toolbox the cluster chart deploys.
func toolboxArgs(command ...string) []string {
	return append([]string{"-n", "rook-ceph", "exec", "deploy/rook-ceph-tools", "--"}, command...)
}

// confUnsafe holds the characters a ceph.conf value cannot carry: values are
// unquoted, so '#' and ';' start a comment, '$' expands a metavariable, '\'
// escapes the next character, and a line break — a lone '\r' included — ends
// the value.
const confUnsafe = "#;$\\\r\n"

// outPaths returns --out made absolute and the keyring path in it, refusing a
// keyring path that ceph.conf could not carry.
func outPaths(out string) (string, string, error) {
	out, err := filepath.Abs(out)
	if err != nil {
		return "", "", err
	}
	keyringPath := filepath.Join(out, "ceph.client.admin.keyring")
	if i := strings.IndexAny(keyringPath, confUnsafe); i >= 0 {
		return "", "", fmt.Errorf("--out %q would put %q in the keyring path, and a ceph.conf value cannot carry "+
			"# ; $ \\ or a line break (they start a comment, expand a variable, escape, or end the value); "+
			"choose a plainer --out", out, keyringPath[i])
	}
	return out, keyringPath, nil
}

// exportCephConfig writes ceph.conf and the client.admin keyring into out,
// creating it, and returns the conf's absolute path. Everything is read from
// the cluster before anything is written, so a failed read leaves out
// untouched.
func exportCephConfig(out string, warn io.Writer) (string, error) {
	out, keyringPath, err := outPaths(out)
	if err != nil {
		return "", err
	}
	clusters, err := kubectlOutput("-n", "rook-ceph", "get", "cephcluster", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("read the CephCluster: %w", err)
	}
	if err := requireHostNetwork(clusters); err != nil {
		return "", err
	}
	dump, err := kubectlOutput(toolboxArgs("ceph", "mon", "dump", "-f", "json")...)
	if err != nil {
		return "", fmt.Errorf("read the mon addresses: %w", err)
	}
	monHosts, err := parseMonHosts(dump)
	if err != nil {
		return "", err
	}
	keyring, err := kubectlOutput(toolboxArgs("ceph", "auth", "get", "client.admin")...)
	if err != nil {
		return "", fmt.Errorf("read the client.admin keyring: %w", err)
	}
	keyType, err := keyringKeyType(keyring)
	if err != nil {
		return "", fmt.Errorf("read the client.admin key's type: %w", err)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	// kubectlOutput trims the newline that ends the keyring.
	if err := writeFileAtomic(keyringPath, []byte(keyring+"\n"), 0o600); err != nil {
		return "", err
	}
	confPath := filepath.Join(out, "ceph.conf")
	if err := writeFileAtomic(confPath, []byte(renderCephConf(monHosts, keyringPath, keyType)), 0o644); err != nil {
		return "", err
	}
	if w := keyTypeWarning(keyType); w != "" {
		fmt.Fprintf(warn, "warning: %s\n", w)
	}
	return confPath, nil
}

// cephClusterList is the part of `kubectl get cephcluster -o json`
// ceph-config reads.
type cephClusterList struct {
	Items []struct {
		Spec struct {
			Network struct {
				Provider    string `json:"provider"`
				HostNetwork bool   `json:"hostNetwork"`
			} `json:"network"`
		} `json:"spec"`
	} `json:"items"`
}

// requireHostNetwork refuses a cluster whose mons listen on pod IPs: the host
// cannot reach them, so a conf naming them could never connect. It takes
// `kubectl get cephcluster -o json` and decides on the first CephCluster, the
// one rooket deploys, as Rook's NetworkSpec.IsHost reads a spec: provider
// "host", or the legacy hostNetwork with no provider.
func requireHostNetwork(list string) error {
	var l cephClusterList
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		return fmt.Errorf("parse the CephCluster list: %w", err)
	}
	if len(l.Items) == 0 {
		return errors.New("there is no CephCluster in rook-ceph (is the cluster deployed?)")
	}
	n := l.Items[0].Spec.Network
	if n.Provider == "host" || (n.HostNetwork && n.Provider == "") {
		return nil
	}
	got := "unset"
	if n.Provider != "" {
		got = fmt.Sprintf("%q", n.Provider)
	}
	return fmt.Errorf("the cluster is not host-networked (its CephCluster's network.provider is %s, not \"host\"), "+
		"so its mons listen on pod IPs the host cannot reach. Rook does not support changing a running cluster's "+
		"network, so recreate it with the host-network profile: rooket down, then rooket up --with host-network, "+
		"and add host-network to the profiles list in the configuration home's config.yaml (a clone's .rooket, "+
		"or --config-dir) so later ups keep it", got)
}

// monDump is the part of `ceph mon dump -f json` ceph-config reads.
type monDump struct {
	Mons []struct {
		Name        string `json:"name"`
		PublicAddrs struct {
			Addrvec []struct {
				Type string `json:"type"`
				Addr string `json:"addr"`
			} `json:"addrvec"`
		} `json:"public_addrs"`
	} `json:"mons"`
}

// parseMonHosts returns each mon's v2 address from `ceph mon dump -f json`,
// spelled as mon_host takes it (v2:IP:PORT), in the dump's order. A mon with
// no v2 address is an error rather than skipped, so the conf never names fewer
// mons than the cluster has without saying why.
func parseMonHosts(dump string) ([]string, error) {
	var d monDump
	if err := json.Unmarshal([]byte(dump), &d); err != nil {
		return nil, fmt.Errorf("parse the mon dump: %w", err)
	}
	if len(d.Mons) == 0 {
		return nil, errors.New("the mon dump lists no mons")
	}
	hosts := make([]string, 0, len(d.Mons))
	for _, m := range d.Mons {
		host := ""
		for _, a := range m.PublicAddrs.Addrvec {
			if a.Type == "v2" {
				host = "v2:" + a.Addr
				break
			}
		}
		if host == "" {
			return nil, fmt.Errorf("mon %q has no v2 address, and ceph-config writes v2 addresses only", m.Name)
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// The CEPH_CRYPTO_* key types, from Ceph's include/ceph_fs.h, that ceph-config
// tells apart; any other is reported by number alone.
const (
	cephCryptoAES        = 1
	cephCryptoAES256KRB5 = 2
)

// keyringKeyType returns the type of a keyring's first key. The key is a
// base64'd CryptoKey encoding, which opens with its type as a little-endian
// uint16.
func keyringKeyType(keyring string) (uint16, error) {
	for line := range strings.Lines(keyring) {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "key" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("decode the key: %w", err)
		}
		if len(raw) < 2 {
			return 0, fmt.Errorf("the key is too short to carry a type (%d bytes)", len(raw))
		}
		return binary.LittleEndian.Uint16(raw), nil
	}
	return 0, errors.New("the keyring has no key")
}

// keyTypeLabel names a key type for ceph.conf's comment and the warning.
func keyTypeLabel(t uint16) string {
	if t == cephCryptoAES {
		return "AES"
	}
	return fmt.Sprintf("not AES (type %d)", t)
}

// keyTypeWarning returns the warning a client.admin key of type t calls for,
// or "" for AES, which every librados can parse.
func keyTypeWarning(t uint16) string {
	const failure = `(such a client logs "auth: ... Malformed input", then rados_connect returns EIO)`
	switch t {
	case cephCryptoAES:
		return ""
	case cephCryptoAES256KRB5:
		return fmt.Sprintf("the client.admin key is AES256KRB5 (type %d), which librados older than "+
			"19.2.6 / 20.2.4 cannot parse %s", t, failure)
	default:
		return fmt.Sprintf("the client.admin key is %s, which a librados that does not know the type "+
			"cannot parse %s", keyTypeLabel(t), failure)
	}
}

// renderCephConf returns a ceph.conf that librados can read by itself.
func renderCephConf(monHosts []string, keyring string, keyType uint16) string {
	return "[global]\n" +
		"mon_host = " + strings.Join(monHosts, ",") + "\n" +
		"# client.admin key type: " + keyTypeLabel(keyType) + "\n" +
		"[client.admin]\n" +
		"keyring = " + keyring + "\n"
}

func init() {
	rootCmd.AddCommand(cephConfigCmd)
	cephConfigCmd.Flags().StringVar(&cephConfigName, "name", "", "kind cluster name")
	cephConfigCmd.Flags().StringVar(&cephConfigOut, "out", "", "directory to write ceph.conf and ceph.client.admin.keyring into (created if missing)")
	_ = cephConfigCmd.MarkFlagRequired("out")
}
