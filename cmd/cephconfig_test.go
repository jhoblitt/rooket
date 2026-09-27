package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	clusterQuery = "-n rook-ceph get cephcluster -o json"
	monDumpQuery = "-n rook-ceph exec deploy/rook-ceph-tools -- ceph --connect-timeout=20 --rados-mon-op-timeout=20 mon dump -f json"
	authGetQuery = "-n rook-ceph exec deploy/rook-ceph-tools -- ceph --connect-timeout=20 --rados-mon-op-timeout=20 auth get client.admin"
)

// oneMonDump is `ceph mon dump -f json` for a single host-networked mon, with
// the fields ceph-config does not read left in to show they are tolerated.
const oneMonDump = `{"epoch":1,"fsid":"0d6c3d2e-7a5b-4a8f-9e61-3b1f2c4d5e6f","min_mon_release_name":"squid",` +
	`"mons":[{"rank":0,"name":"a","public_addrs":{"addrvec":[` +
	`{"type":"v2","addr":"10.89.0.5:3300","nonce":0},{"type":"v1","addr":"10.89.0.5:6789","nonce":0}]},` +
	`"addr":"10.89.0.5:6789/0","public_addr":"10.89.0.5:6789/0","priority":0,"weight":0}],"quorum":[0]}`

// cephClusters is `kubectl get cephcluster -o json` listing one CephCluster per
// spec given.
func cephClusters(specs ...string) string {
	items := make([]string, 0, len(specs))
	for i, spec := range specs {
		items = append(items, fmt.Sprintf(`{"apiVersion":"ceph.rook.io/v1","kind":"CephCluster",`+
			`"metadata":{"name":"rook-ceph-%d","namespace":"rook-ceph"},"spec":%s,"status":{"phase":"Ready"}}`, i, spec))
	}
	return `{"apiVersion":"v1","items":[` + strings.Join(items, ",") + `],"kind":"List","metadata":{"resourceVersion":""}}`
}

// cephKey encodes a CryptoKey as Ceph does — a little-endian type, the
// creation time, the secret's length, the secret — and base64s it the way a
// keyring carries it.
func cephKey(typ uint16) string {
	b := binary.LittleEndian.AppendUint16(nil, typ)
	b = binary.LittleEndian.AppendUint32(b, 1_700_000_000)
	b = binary.LittleEndian.AppendUint32(b, 0)
	secret := bytes.Repeat([]byte{0xab}, 16)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(secret)))
	return base64.StdEncoding.EncodeToString(append(b, secret...))
}

func adminKeyring(key string) string {
	return "[client.admin]\n\tkey = " + key + "\n" +
		"\tcaps mds = \"allow *\"\n\tcaps mgr = \"allow *\"\n\tcaps mon = \"allow *\"\n\tcaps osd = \"allow *\""
}

// stubKubectl answers kubectlOutput from canned stdout keyed by the full
// argument line, failing any command it has no answer for, and returns the
// lines it was asked to run. It also fails a query whose context carries no
// deadline within ceph-config's budget, so an unbounded query fails the test.
func stubKubectl(t *testing.T, answers map[string]string) *[]string {
	t.Helper()
	prev := kubectlOutput
	var calls []string
	kubectlOutput = func(ctx context.Context, args ...string) (string, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		now := time.Now()
		if deadline, ok := ctx.Deadline(); !ok || !deadline.After(now) || deadline.After(now.Add(cephConfigQueryBudget)) {
			t.Errorf("kubectl %s ran with deadline %v (set: %v), want one within %s from now",
				line, deadline, ok, cephConfigQueryBudget)
			return "", errors.New("no budget")
		}
		if out, ok := answers[line]; ok {
			return out, nil
		}
		return "", fmt.Errorf("unexpected kubectl %s", line)
	}
	t.Cleanup(func() { kubectlOutput = prev })
	return &calls
}

// hostNetworked answers every query ceph-config makes for a healthy
// host-networked cluster whose admin key has the given type.
func hostNetworked(keyType uint16) map[string]string {
	return map[string]string{
		clusterQuery: cephClusters(`{"network":{"provider":"host"}}`),
		monDumpQuery: oneMonDump,
		authGetQuery: adminKeyring(cephKey(keyType)),
	}
}

// Host networking is decided as Rook's NetworkSpec.IsHost decides it, so a
// cluster using the legacy hostNetwork field is not refused.
func TestRequireHostNetwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		list    string
		wantErr string
	}{
		{name: "provider host", list: cephClusters(`{"network":{"provider":"host"}}`)},
		{name: "legacy hostNetwork with no provider", list: cephClusters(`{"network":{"hostNetwork":true}}`)},
		{name: "legacy hostNetwork with an empty provider", list: cephClusters(`{"network":{"provider":"","hostNetwork":true}}`)},
		{name: "provider multus", list: cephClusters(`{"network":{"provider":"multus"}}`), wantErr: "host-network"},
		{name: "legacy hostNetwork beside another provider", list: cephClusters(`{"network":{"provider":"multus","hostNetwork":true}}`), wantErr: "host-network"},
		{name: "provider unset and hostNetwork false", list: cephClusters(`{"network":{"hostNetwork":false}}`), wantErr: "host-network"},
		{name: "no network section", list: cephClusters(`{}`), wantErr: "host-network"},
		{name: "the first of several decides", list: cephClusters(`{"network":{"provider":"host"}}`, `{}`)},
		{name: "no CephCluster", list: cephClusters(), wantErr: "no CephCluster in rook-ceph"},
		{name: "not JSON", list: "error: the server doesn't have a resource type", wantErr: "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireHostNetwork("c", tc.list)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("requireHostNetwork() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("requireHostNetwork() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseMonHosts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dump    string
		want    []string
		wantErr string
	}{
		{name: "one mon", dump: oneMonDump, want: []string{"v2:10.89.0.5:3300"}},
		{
			name: "several mons, in dump order, whatever order each lists its addresses in",
			dump: `{"mons":[` +
				`{"name":"a","public_addrs":{"addrvec":[{"type":"v2","addr":"10.89.0.5:3300","nonce":0},{"type":"v1","addr":"10.89.0.5:6789","nonce":0}]}},` +
				`{"name":"b","public_addrs":{"addrvec":[{"type":"v1","addr":"10.89.0.6:6789","nonce":0},{"type":"v2","addr":"10.89.0.6:3300","nonce":0}]}},` +
				`{"name":"c","public_addrs":{"addrvec":[{"type":"v2","addr":"10.89.0.7:3300","nonce":0}]}}]}`,
			want: []string{"v2:10.89.0.5:3300", "v2:10.89.0.6:3300", "v2:10.89.0.7:3300"},
		},
		{
			name: "a mon with only a v1 address",
			dump: `{"mons":[` +
				`{"name":"a","public_addrs":{"addrvec":[{"type":"v2","addr":"10.89.0.5:3300","nonce":0}]}},` +
				`{"name":"b","public_addrs":{"addrvec":[{"type":"v1","addr":"10.89.0.6:6789","nonce":0}]}}]}`,
			wantErr: `mon "b" has no v2 address`,
		},
		{name: "no mons", dump: `{"mons":[]}`, wantErr: "no mons"},
		{name: "not JSON", dump: "dumped monmap epoch 1", wantErr: "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMonHosts(tc.dump)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseMonHosts() = %v, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMonHosts: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("parseMonHosts() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyringKeyType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keyring   string
		wantType  uint16
		wantLabel string
		wantErr   string
	}{
		{name: "AES", keyring: adminKeyring(cephKey(1)), wantType: 1, wantLabel: "AES"},
		{name: "another type", keyring: adminKeyring(cephKey(2)), wantType: 2, wantLabel: "not AES (type 2)"},
		{name: "malformed base64", keyring: adminKeyring("AQ!!not base64"), wantErr: "decode"},
		{name: "too short to hold a type", keyring: adminKeyring(base64.StdEncoding.EncodeToString([]byte{1})), wantErr: "too short"},
		{name: "no key line", keyring: "[client.admin]\n\tcaps mon = \"allow *\"", wantErr: "no key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keyringKeyType(tc.keyring)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("keyringKeyType() = %d, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("keyringKeyType: %v", err)
			}
			if got != tc.wantType {
				t.Errorf("keyringKeyType() = %d, want %d", got, tc.wantType)
			}
			if l := keyTypeLabel(got); l != tc.wantLabel {
				t.Errorf("keyTypeLabel(%d) = %q, want %q", got, l, tc.wantLabel)
			}
		})
	}
}

func TestRenderCephConf(t *testing.T) {
	got := renderCephConf([]string{"v2:10.89.0.5:3300", "v2:10.89.0.6:3300"}, "/out/ceph.client.admin.keyring", 2)
	want := `[global]
mon_host = v2:10.89.0.5:3300,v2:10.89.0.6:3300
# client.admin key type: not AES (type 2)
[client.admin]
keyring = /out/ceph.client.admin.keyring
`
	if got != want {
		t.Errorf("renderCephConf() =\n%s\nwant\n%s", got, want)
	}
}

// Pod IPs are unreachable from the host, so a conf naming them could never
// connect: ceph-config refuses before it runs anything in the toolbox or
// writes anything.
func TestExportCephConfigRefusesAClusterThatIsNotHostNetworked(t *testing.T) {
	for _, provider := range []string{"", "multus"} {
		t.Run("provider="+provider, func(t *testing.T) {
			answers := hostNetworked(1)
			answers[clusterQuery] = cephClusters(fmt.Sprintf(`{"network":{"provider":%q}}`, provider))
			calls := stubKubectl(t, answers)
			out := filepath.Join(t.TempDir(), "ceph")

			_, err := exportCephConfig("c", out, &bytes.Buffer{})
			if err == nil {
				t.Fatal("exportCephConfig() succeeded, want a refusal naming the host-network profile")
			}
			// Rook does not support changing a running cluster's network, so
			// the advice is to recreate it host-networked and keep it so, naming
			// the cluster, since a bare down could pick another one; the
			// profile is added to the sticky list rather than replacing it,
			// and a cluster with no configuration home can get one from
			// --config-dir.
			msg := err.Error()
			for _, want := range []string{"rooket down --name c", "rooket up --name c --with host-network",
				"add host-network to the profiles list", "config.yaml", "--config-dir"} {
				if !strings.Contains(msg, want) {
					t.Errorf("exportCephConfig() = %v, want the refusal to say %q", err, want)
				}
			}
			if down, up := strings.Index(msg, "rooket down --name c"), strings.Index(msg, "rooket up --name c --with host-network"); down > up {
				t.Errorf("exportCephConfig() = %v, want the cluster taken down before it is brought up host-networked", err)
			}
			for _, bad := range []string{"profiles: [", "for one run"} {
				if strings.Contains(msg, bad) {
					t.Errorf("exportCephConfig() = %v, want no %q", err, bad)
				}
			}
			if len(*calls) != 1 {
				t.Errorf("ran %q, want only the CephCluster query", *calls)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("--out was created (stat: %v); a refusal must write nothing", err)
			}
		})
	}
}

func TestExportCephConfigWritesConfAndKeyring(t *testing.T) {
	stubKubectl(t, hostNetworked(1))
	out := filepath.Join(t.TempDir(), "missing", "ceph")
	var warn bytes.Buffer

	conf, err := exportCephConfig("c", out, &warn)
	if err != nil {
		t.Fatalf("exportCephConfig: %v", err)
	}

	fi, err := os.Stat(out)
	if err != nil || !fi.IsDir() {
		t.Fatalf("--out not created as a directory: %v", err)
	}
	if extra := fi.Mode().Perm() &^ 0o755; extra != 0 {
		t.Errorf("--out mode = %v, grants more than 0755", fi.Mode().Perm())
	}
	keyring := filepath.Join(out, "ceph.client.admin.keyring")
	assertFile(t, keyring, 0o600, adminKeyring(cephKey(1))+"\n")
	if conf != filepath.Join(out, "ceph.conf") {
		t.Errorf("exportCephConfig() = %q, want the conf's path", conf)
	}
	assertFile(t, conf, 0o644,
		renderCephConf([]string{"v2:10.89.0.5:3300"}, keyring, 1))
	if warn.Len() != 0 {
		t.Errorf("warned %q for an AES key", warn.String())
	}
}

// A relative --out still puts an absolute keyring path in the conf, so the
// conf does not depend on the directory the client runs in.
func TestExportCephConfigNamesTheKeyringByAbsolutePath(t *testing.T) {
	stubKubectl(t, hostNetworked(1))
	dir := t.TempDir()
	t.Chdir(dir)

	path, err := exportCephConfig("c", "rel", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("exportCephConfig: %v", err)
	}
	if want := filepath.Join(dir, "rel", "ceph.conf"); path != want {
		t.Errorf("exportCephConfig() = %q, want %q", path, want)
	}
	conf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "keyring = " + filepath.Join(dir, "rel", "ceph.client.admin.keyring") + "\n"
	if !strings.Contains(string(conf), want) {
		t.Errorf("ceph.conf =\n%s\nwant a line %q", conf, want)
	}
}

// A keyring left readable by an earlier write is tightened, not kept: the file
// is replaced rather than rewritten in place.
func TestExportCephConfigReplacesAWiderKeyring(t *testing.T) {
	stubKubectl(t, hostNetworked(1))
	out := t.TempDir()
	keyring := filepath.Join(out, "ceph.client.admin.keyring")
	if err := os.WriteFile(keyring, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyring, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := exportCephConfig("c", out, &bytes.Buffer{}); err != nil {
		t.Fatalf("exportCephConfig: %v", err)
	}
	assertFile(t, keyring, 0o600, adminKeyring(cephKey(1))+"\n")
}

func TestExportCephConfigWarnsOnANonAESKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keyType  uint16
		want     []string
		dontWant []string
	}{
		{
			name:     "AES256KRB5",
			keyType:  2,
			want:     []string{"AES256KRB5 (type 2)", "19.2.6", "20.2.4", "Malformed input", "EIO"},
			dontWant: []string{"likely"},
		},
		{
			name:     "a type ceph-config has no name for",
			keyType:  7,
			want:     []string{"not AES (type 7)", "Malformed input", "EIO"},
			dontWant: []string{"AES256KRB5"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubKubectl(t, hostNetworked(tc.keyType))
			out := t.TempDir()
			var warn bytes.Buffer

			if _, err := exportCephConfig("c", out, &warn); err != nil {
				t.Fatalf("exportCephConfig: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(warn.String(), want) {
					t.Errorf("warning %q does not mention %q", warn.String(), want)
				}
			}
			for _, bad := range tc.dontWant {
				if strings.Contains(warn.String(), bad) {
					t.Errorf("warning %q says %q", warn.String(), bad)
				}
			}
			conf, err := os.ReadFile(filepath.Join(out, "ceph.conf"))
			if err != nil {
				t.Fatal(err)
			}
			comment := fmt.Sprintf("# client.admin key type: not AES (type %d)\n", tc.keyType)
			if !strings.Contains(string(conf), comment) {
				t.Errorf("ceph.conf =\n%s\nwant a line %q", conf, comment)
			}
		})
	}
}

// ceph.conf values are unquoted, so a keyring path holding one of these would
// be cut short, expanded, or unescaped when librados reads it back: refused
// before anything is queried or written.
func TestExportCephConfigRefusesAnOutTheConfCannotCarry(t *testing.T) {
	for _, c := range []string{"#", ";", "$", `\`, "\n", "\r"} {
		t.Run(fmt.Sprintf("%q", c), func(t *testing.T) {
			calls := stubKubectl(t, hostNetworked(1))
			out := filepath.Join(t.TempDir(), "we"+c+"ird")

			_, err := exportCephConfig("c", out, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "--out") {
				t.Fatalf("exportCephConfig(%q) = %v, want an error about --out", out, err)
			}
			if len(*calls) != 0 {
				t.Errorf("ran %q, want nothing queried", *calls)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("--out was created (stat: %v) though it was refused", err)
			}
		})
	}
}

// Every query runs before the first write, so one that fails leaves --out as
// it was.
func TestExportCephConfigWritesNothingWhenAQueryFails(t *testing.T) {
	for _, drop := range []string{clusterQuery, monDumpQuery, authGetQuery} {
		t.Run(drop, func(t *testing.T) {
			answers := hostNetworked(1)
			delete(answers, drop)
			stubKubectl(t, answers)
			out := filepath.Join(t.TempDir(), "ceph")

			if _, err := exportCephConfig("c", out, &bytes.Buffer{}); err == nil {
				t.Fatal("exportCephConfig succeeded with a failing query")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("--out was created (stat: %v) though a query failed", err)
			}
		})
	}
}

// A query that outlasts its budget fails ceph-config naming the budget, not
// the "signal: killed" kubectl died of, and leaves --out as it was.
func TestExportCephConfigNamesAQueryItsBudgetCutOff(t *testing.T) {
	stubKubectl(t, hostNetworked(1))
	stubbed := kubectlOutput
	kubectlOutput = func(ctx context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") == monDumpQuery {
			return "", fmt.Errorf("%w: signal: killed", context.DeadlineExceeded)
		}
		return stubbed(ctx, args...)
	}
	out := filepath.Join(t.TempDir(), "ceph")

	_, err := exportCephConfig("c", out, &bytes.Buffer{})
	want := fmt.Sprintf("read the mon addresses: timed out: its %s budget ran out", cephConfigQueryBudget)
	if err == nil || err.Error() != want {
		t.Fatalf("exportCephConfig() = %v, want %q", err, want)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--out was created (stat: %v) though a query timed out", err)
	}
}

// Killing kubectl at the end of a budget leaves what it started in the
// toolbox running, so each ceph command there carries its own bound.
func TestExportCephConfigBoundsWhatRunsInTheToolbox(t *testing.T) {
	calls := stubKubectl(t, hostNetworked(1))

	if _, err := exportCephConfig("c", t.TempDir(), &bytes.Buffer{}); err != nil {
		t.Fatalf("exportCephConfig: %v", err)
	}
	ran := 0
	for _, c := range *calls {
		if _, command, inToolbox := strings.Cut(c, " -- "); inToolbox {
			ran++
			if !strings.HasPrefix(command, "ceph --connect-timeout=20 --rados-mon-op-timeout=20 ") {
				t.Errorf("ran %q, want ceph bounded by --connect-timeout and --rados-mon-op-timeout", command)
			}
		}
	}
	if ran != 2 {
		t.Errorf("ran %q, want the mon dump and the auth get in the toolbox", *calls)
	}
}

func TestCephConfigCmdTargetsTheSelectedCluster(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env, want string
	}{
		{name: "--name", flag: "alpha", env: "beta", want: "alpha"},
		// Not "$ROOKET_NAME": t.TempDir keeps the '$' from a subtest's name,
		// and --out may not hold one.
		{name: "ROOKET_NAME", env: "beta", want: "beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ROOKET_NAME", tc.env)
			t.Setenv("KUBECONFIG", "")
			setCephConfigFlags(t, tc.flag, t.TempDir())
			kc := writeKubeconfig(t, tc.want)
			var seen []string
			stubKubectl(t, hostNetworked(1))
			stubbed := kubectlOutput
			kubectlOutput = func(ctx context.Context, args ...string) (string, error) {
				seen = append(seen, os.Getenv("KUBECONFIG"))
				return stubbed(ctx, args...)
			}

			if err := cephConfigCmd.RunE(cephConfigCmd, nil); err != nil {
				t.Fatalf("ceph-config: %v", err)
			}
			if len(seen) == 0 {
				t.Fatal("ran no kubectl")
			}
			for _, got := range seen {
				if got != kc {
					t.Errorf("kubectl ran with KUBECONFIG=%q, want %q", got, kc)
				}
			}
		})
	}
}

func TestCephConfigCmdNeedsTheClusterUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	setCephConfigFlags(t, "alpha", t.TempDir())
	calls := stubKubectl(t, hostNetworked(1))

	err := cephConfigCmd.RunE(cephConfigCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "is it up?") {
		t.Fatalf("ceph-config without a kubeconfig = %v, want an is-it-up error", err)
	}
	if len(*calls) != 0 {
		t.Errorf("ran %q against a cluster with no kubeconfig", *calls)
	}
}

// --out is checked before the cluster, as wait checks --timeout: a bad one is
// reported whether or not the cluster is up, rather than after it is.
func TestCephConfigCmdChecksOutBeforeTheCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "untouched")
	setCephConfigFlags(t, "alpha", filepath.Join(t.TempDir(), "we#ird"))
	calls := stubKubectl(t, hostNetworked(1))

	err := cephConfigCmd.RunE(cephConfigCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--out") || strings.Contains(err.Error(), "is it up?") {
		t.Fatalf("ceph-config with a bad --out and no cluster = %v, want the --out refused", err)
	}
	if len(*calls) != 0 {
		t.Errorf("ran %q for a refused --out", *calls)
	}
	if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
		t.Errorf("KUBECONFIG = %q: the cluster was resolved before --out was checked", kc)
	}
}

// The key-type warning goes where the command's errors go, so whoever runs
// the command can capture it.
func TestCephConfigCmdWarnsOnItsErrorStream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	setCephConfigFlags(t, "alpha", t.TempDir())
	writeKubeconfig(t, "alpha")
	stubKubectl(t, hostNetworked(cephCryptoAES256KRB5))
	var errOut bytes.Buffer
	cephConfigCmd.SetErr(&errOut)
	t.Cleanup(func() { cephConfigCmd.SetErr(nil) })

	if err := cephConfigCmd.RunE(cephConfigCmd, nil); err != nil {
		t.Fatalf("ceph-config: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning: the client.admin key is AES256KRB5") {
		t.Errorf("the command's error stream got %q, want the key-type warning", errOut.String())
	}
}

func setCephConfigFlags(t *testing.T, name, out string) {
	t.Helper()
	prevName, prevOut := cephConfigName, cephConfigOut
	cephConfigName, cephConfigOut = name, out
	t.Cleanup(func() { cephConfigName, cephConfigOut = prevName, prevOut })
}

func assertFile(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != mode {
		t.Errorf("%s mode = %v, want %v", filepath.Base(path), fi.Mode().Perm(), mode)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("%s =\n%s\nwant\n%s", filepath.Base(path), got, content)
	}
}
