package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	waitName    string
	waitTimeout time.Duration
)

const (
	defaultWaitTimeout = 20 * time.Minute
	waitPollInterval   = 10 * time.Second
	// waitPollBudget bounds one poll's queries together, and waitDiagBudget
	// each diagnostic, so a wedged apiserver or toolbox cannot hold the wait
	// far past its timeout.
	waitPollBudget = time.Minute
	waitDiagBudget = 30 * time.Second
	// readyPhase is the status.phase Rook gives a CephCluster or
	// CephObjectStore once it has reconciled it.
	readyPhase = "Ready"
)

var waitCmd = &cobra.Command{
	Use:   "wait",
	Short: "Wait until the cluster is ready for clients",
	Long: `wait blocks until the cluster's Ceph is ready for clients and exits 0, or
exits non-zero once --timeout passes. Ready means all of:

  - the CephCluster's phase is Ready;
  - there is an OSD, and every OSD is up and in;
  - there is a PG, and every PG is active and clean with its IO flowing (a
    PG that is also, say, scrubbing still serves clients, so it counts; one
    that is stale, laggy, waiting, or premerge does not);
  - every CephObjectStore in the rook-ceph namespace is Ready, and its RGW
    answers HTTP from inside the Ceph toolbox. A cluster with no object store
    needs none.

HEALTH_OK is not required: a cluster on a single worker settles at HEALTH_WARN
and serves clients all the same.

wait checks every 10 seconds and prints a line whenever what it is still
waiting for changes. At the timeout it names each unmet condition and prints,
to stderr, ceph status, ceph health detail, ceph osd tree, and every rook-ceph
pod that is neither Completed nor Running with all its containers ready. A
check that hangs is cut off after a minute, and each of those diagnostics
after 30 seconds, so wait ends no more than about a minute past --timeout,
plus the diagnostics.

The cluster is selected the same way as the rest of rooket: --name, else
$ROOKET_NAME, else the name derived from the enclosing rook clone's path.
'rooket up --wait' runs the same wait once it has deployed.

  rooket wait
  rooket wait --name mycluster --timeout 30m
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if waitTimeout <= 0 {
			return fmt.Errorf("--timeout must be more than 0, not %s", waitTimeout)
		}
		name, err := useCluster(waitName)
		if err != nil {
			return err
		}
		return waitForCluster(name, waitTimeout, cmd.ErrOrStderr())
	},
}

// waitKubectl runs the wait's kubectl queries, each bounded by its ctx.
// Indirected, as kubectlOutput is, so tests can answer with canned output;
// untraced, because the queries repeat every poll and the wait prints only
// when what it waits for changes.
var waitKubectl = func(ctx context.Context, args ...string) (string, error) {
	return runKubectlContextTo(ctx, io.Discard, args...)
}

// waitForCluster waits until the named cluster, the one $KUBECONFIG already
// points at (see useCluster), is ready for clients or timeout passes, printing
// progress to stdout and, at a timeout, diagnostics to diag.
func waitForCluster(name string, timeout time.Duration, diag io.Writer) error {
	if _, err := requireKubeconfig(name); err != nil {
		return err
	}
	w := readyWaiter{
		kubectl:    waitKubectl,
		now:        time.Now,
		sleep:      time.Sleep,
		interval:   waitPollInterval,
		pollBudget: waitPollBudget,
		diagBudget: waitDiagBudget,
		out:        os.Stdout,
		diag:       diag,
	}
	return w.wait(name, timeout)
}

// readyWaiter polls a cluster until it is ready for clients. It reaches the
// cluster and the clock only through its fields, so tests run it against
// canned kubectl output on a clock that never really sleeps.
type readyWaiter struct {
	kubectl  func(ctx context.Context, args ...string) (string, error)
	now      func() time.Time
	sleep    func(time.Duration)
	interval time.Duration
	// pollBudget bounds each poll's queries together, diagBudget each
	// diagnostic.
	pollBudget, diagBudget time.Duration
	// out takes the progress lines, diag the diagnostics printed at a timeout.
	out, diag io.Writer
}

// wait polls every interval until the cluster is ready or timeout passes,
// printing a line whenever the unmet conditions change. The timeout is checked
// between polls, and the last sleep is cut short so the final poll starts on
// the deadline. A poll still running then finishes within its own budget,
// which is not trimmed to the time left: trimmed, the last poll's unmet list
// would say only that it ran out of time. At the deadline wait prints the
// diagnostics and returns an error naming each unmet condition.
func (w readyWaiter) wait(name string, timeout time.Duration) error {
	start := w.now()
	deadline := start.Add(timeout)
	run.Fprintf(w.out, "==> waiting up to %s for cluster %q to be ready for clients\n", timeout, name)
	var last []string
	for {
		unmet := unmetConditions(w.observe())
		if len(unmet) == 0 {
			run.Fprintf(w.out, "==> cluster %q is ready for clients after %s\n", name, fmtDur(w.now().Sub(start)))
			return nil
		}
		if !slices.Equal(unmet, last) {
			run.Fprintf(w.out, "==> not ready: %s\n", strings.Join(unmet, "; "))
			last = unmet
		}
		left := deadline.Sub(w.now())
		if left <= 0 {
			took := fmtDur(w.now().Sub(start))
			w.diagnose()
			return fmt.Errorf("cluster %q is not ready for clients after %s (timeout %s); unmet:\n  - %s",
				name, took, timeout, strings.Join(unmet, "\n  - "))
		}
		w.sleep(min(w.interval, left))
	}
}

// observe reads the cluster once, its queries sharing one pollBudget. A query
// that fails is recorded rather than returned: until the cluster settles —
// while the toolbox pod is still unscheduled, say — a failing query is one
// more condition not met yet, and so is one the budget cuts off.
func (w readyWaiter) observe() readiness {
	ctx, cancel := w.budget(w.pollBudget)
	defer cancel()
	spent := fmt.Sprintf("the poll's %s budget", w.pollBudget)
	var r readiness
	if out, err := w.query(ctx, spent, "-n", "rook-ceph", "get", "cephcluster", "-o", "json"); err != nil {
		r.phaseErr = err
	} else {
		r.phase, r.phaseErr = parseCephClusterPhase(out)
	}
	if out, err := w.query(ctx, spent, toolboxCeph("status", "-f", "json")...); err != nil {
		r.statusErr = err
	} else {
		r.status, r.statusErr = parseCephStatus(out)
	}
	if out, err := w.query(ctx, spent, "-n", "rook-ceph", "get", "cephobjectstore", "-o", "json"); err != nil {
		r.storesErr = err
	} else {
		r.stores, r.storesErr = parseObjectStores(out)
	}
	r.rgw = map[string]rgwProbe{}
	for _, s := range r.stores {
		if s.phase != readyPhase || s.endpoint == "" {
			continue
		}
		// -k because the probe asks whether the RGW answers HTTP, not whether
		// its certificate is trusted. exec runs curl without a shell, so the -w
		// format needs no quoting.
		code, err := w.query(ctx, spent, toolboxArgs("curl", "-s", "-k", "--max-time", "10",
			"-o", "/dev/null", "-w", "%{http_code}", s.endpoint)...)
		r.rgw[s.name] = rgwProbe{code: code, err: err}
	}
	return r
}

// budget returns a context that ends d from now on the waiter's clock, which
// is time.Now outside tests.
func (w readyWaiter) budget(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), w.now().Add(d))
}

// query runs one kubectl query under ctx. One that ctx cut off fails naming
// the budget that ran out, spent, rather than the "signal: killed" kubectl
// died of.
func (w readyWaiter) query(ctx context.Context, spent string, args ...string) (string, error) {
	out, err := w.kubectl(ctx, args...)
	if errors.Is(err, context.DeadlineExceeded) {
		return out, fmt.Errorf("timed out: %s ran out", spent)
	}
	return out, err
}

// toolboxCeph returns the kubectl arguments that run a ceph command in the
// toolbox, bounded so that ceph gives up by itself. Killing kubectl at the end
// of a budget leaves the ceph it started running, and unbounded, ceph waits
// five minutes to connect and as long as it takes for a mon to answer. Each
// option is one argument, so the ceph CLI cannot take its value for a word
// of the command.
func toolboxCeph(command ...string) []string {
	return toolboxArgs(append([]string{"ceph", "--connect-timeout=20", "--rados-mon-op-timeout=20"}, command...)...)
}

// diagnose prints what explains a cluster that did not become ready, each
// command within its own diagBudget. A command that fails is noted and the
// rest still run.
func (w readyWaiter) diagnose() {
	spent := fmt.Sprintf("its %s budget", w.diagBudget)
	for _, d := range []struct {
		title  string
		args   []string
		filter func(string) string
	}{
		{title: "ceph status", args: toolboxCeph("status")},
		{title: "ceph health detail", args: toolboxCeph("health", "detail")},
		{title: "ceph osd tree", args: toolboxCeph("osd", "tree")},
		{
			title:  "rook-ceph pods neither ready nor Completed",
			args:   []string{"-n", "rook-ceph", "get", "pods", "-o", "wide"},
			filter: podsNotReady,
		},
	} {
		fmt.Fprintf(w.diag, "--- %s\n", d.title)
		ctx, cancel := w.budget(w.diagBudget)
		out, err := w.query(ctx, spent, d.args...)
		cancel()
		if err != nil {
			fmt.Fprintf(w.diag, "(failed: %v)\n", err)
			continue
		}
		if d.filter != nil {
			out = d.filter(out)
		}
		if out == "" {
			out = "(none)"
		}
		fmt.Fprintln(w.diag, out)
	}
}

// readiness is one poll's reading of the cluster: what each query found, or
// why it failed.
type readiness struct {
	phase     string
	phaseErr  error
	status    cephStatus
	statusErr error
	stores    []objectStore
	storesErr error
	// rgw holds the probe of each Ready object store's RGW, by store name.
	rgw map[string]rgwProbe
}

// rgwProbe is what curl, run in the toolbox, made of an RGW endpoint.
type rgwProbe struct {
	code string
	err  error
}

// detail says how a probe that found no answering RGW went.
func (p rgwProbe) detail() string {
	switch {
	case p.err == nil && p.code == "":
		return "no HTTP code"
	case p.err == nil:
		return "HTTP " + p.code
	case p.code == "":
		return shortErr(p.err)
	default:
		return "HTTP " + p.code + "; " + shortErr(p.err)
	}
}

// unmetConditions decides, from one poll's reading, each condition that keeps
// the cluster from being ready for clients; none means it is ready.
func unmetConditions(r readiness) []string {
	var unmet []string
	switch {
	case errors.Is(r.phaseErr, errNoCephCluster):
		unmet = append(unmet, r.phaseErr.Error())
	case r.phaseErr != nil:
		unmet = append(unmet, "CephCluster: "+shortErr(r.phaseErr))
	case r.phase == "":
		unmet = append(unmet, "CephCluster has no phase yet")
	case r.phase != readyPhase:
		unmet = append(unmet, fmt.Sprintf("CephCluster phase is %q, not Ready", r.phase))
	}
	if r.statusErr != nil {
		unmet = append(unmet, "ceph status: "+shortErr(r.statusErr))
	} else {
		unmet = append(unmet, cephStatusUnmet(r.status)...)
	}
	if r.storesErr != nil {
		unmet = append(unmet, "object stores: "+shortErr(r.storesErr))
	}
	for _, s := range r.stores {
		switch {
		case s.phase == "":
			unmet = append(unmet, fmt.Sprintf("object store %q has no phase yet", s.name))
		case s.phase != readyPhase:
			unmet = append(unmet, fmt.Sprintf("object store %q phase is %q, not Ready", s.name, s.phase))
		case s.endpoint == "":
			unmet = append(unmet, fmt.Sprintf("object store %q has no endpoint yet", s.name))
		default:
			if p := r.rgw[s.name]; p.err != nil || !rgwAnswered(p.code) {
				unmet = append(unmet, fmt.Sprintf("object store %q: RGW at %s did not answer (%s)",
					s.name, s.endpoint, p.detail()))
			}
		}
	}
	return unmet
}

// errNoCephCluster is a CephCluster list with nothing in it: the cluster
// chart has not created one yet.
var errNoCephCluster = errors.New("no CephCluster in rook-ceph")

// parseCephClusterPhase returns the phase of the first CephCluster in
// `kubectl get cephcluster -o json`, the one rooket deploys: "" until Rook
// sets one.
func parseCephClusterPhase(list string) (string, error) {
	var l cephClusterList
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		return "", fmt.Errorf("cannot parse the output: %w", err)
	}
	if len(l.Items) == 0 {
		return "", errNoCephCluster
	}
	return l.Items[0].Status.Phase, nil
}

// cephStatus is the part of `ceph status -f json` the wait reads.
type cephStatus struct {
	OSDMap struct {
		NumOSDs   int `json:"num_osds"`
		NumUpOSDs int `json:"num_up_osds"`
		NumInOSDs int `json:"num_in_osds"`
	} `json:"osdmap"`
	PGMap struct {
		NumPGs     int `json:"num_pgs"`
		PGsByState []struct {
			StateName string `json:"state_name"`
			Count     int    `json:"count"`
		} `json:"pgs_by_state"`
	} `json:"pgmap"`
}

func parseCephStatus(s string) (cephStatus, error) {
	var st cephStatus
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return cephStatus{}, fmt.Errorf("cannot parse the output: %w", err)
	}
	return st, nil
}

// cephStatusUnmet returns what in `ceph status` keeps the cluster from being
// ready: no OSD, an OSD that is not up or not in, no PG, or a PG that is not
// active and clean. Health is not read — a single-worker cluster settles at
// HEALTH_WARN, and serves clients anyway.
func cephStatusUnmet(st cephStatus) []string {
	var unmet []string
	switch o := st.OSDMap; {
	case o.NumOSDs == 0:
		unmet = append(unmet, "no OSDs")
	case o.NumUpOSDs != o.NumOSDs || o.NumInOSDs != o.NumOSDs:
		unmet = append(unmet, fmt.Sprintf("OSDs: %d up and %d in of %d", o.NumUpOSDs, o.NumInOSDs, o.NumOSDs))
	}
	p := st.PGMap
	if p.NumPGs == 0 {
		return append(unmet, "no PGs")
	}
	clean := 0
	for _, s := range p.PGsByState {
		if pgActiveClean(s.StateName) {
			clean += s.Count
		}
	}
	if clean != p.NumPGs {
		unmet = append(unmet, fmt.Sprintf("PGs: %d of %d active+clean", clean, p.NumPGs))
	}
	return unmet
}

// pgActiveClean reports whether a PG in state serves clients as a settled
// cluster's do: active and clean, and not paused. A scrub, a remap, a snap
// trim, or a repair goes on while the PG keeps serving, and scrubs are
// routine; but Ceph documents IO as paused on a laggy or waiting PG, a stale
// one's state is not known, and a premerge PG holds IO until its merge. The
// state is compared as whole +-separated tokens, never as a prefix or
// substring.
func pgActiveClean(state string) bool {
	tokens := strings.Split(state, "+")
	return slices.Contains(tokens, "active") && slices.Contains(tokens, "clean") &&
		!slices.ContainsFunc(tokens, func(t string) bool { return slices.Contains(pgPausedStates, t) })
}

// pgPausedStates are the PG states that stop an active and clean PG serving.
var pgPausedStates = []string{"stale", "laggy", "wait", "premerge"}

// objectStore is what the wait reads of a CephObjectStore.
type objectStore struct {
	name, phase, endpoint string
}

// cephObjectStoreList is the part of `kubectl get cephobjectstore -o json`
// the wait reads.
type cephObjectStoreList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
			Info  struct {
				Endpoint string `json:"endpoint"`
			} `json:"info"`
		} `json:"status"`
	} `json:"items"`
}

func parseObjectStores(list string) ([]objectStore, error) {
	var l cephObjectStoreList
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		return nil, fmt.Errorf("cannot parse the output: %w", err)
	}
	stores := make([]objectStore, 0, len(l.Items))
	for _, it := range l.Items {
		stores = append(stores, objectStore{name: it.Metadata.Name, phase: it.Status.Phase, endpoint: it.Status.Info.Endpoint})
	}
	return stores, nil
}

// rgwAnswered reports whether curl's %{http_code} is a status an HTTP server
// sent. curl prints 000 when nothing answered.
func rgwAnswered(code string) bool {
	if len(code) != 3 {
		return false
	}
	n, err := strconv.Atoi(code)
	return err == nil && n >= 100 && n <= 599
}

var (
	exitStatusPrefix  = regexp.MustCompile(`^exit status \d+: `)
	commandTerminated = regexp.MustCompile(`^command terminated with exit code \d+$`)
)

// shortErrMax bounds a failure's length in the unmet list, which is printed
// on one line.
const shortErrMax = 160

// shortErr condenses a failed query's error to one line for the unmet list.
// runKubectl's error opens with kubectl's exit status, and a failed exec's
// stderr can run to many lines of Ceph client logging before kubectl's closing
// "command terminated with exit code N": the last line ahead of that is the
// one that says what went wrong.
func shortErr(err error) string {
	msg := exitStatusPrefix.ReplaceAllLiteralString(err.Error(), "")
	var last, cause string
	for line := range strings.Lines(msg) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		if !commandTerminated.MatchString(line) {
			cause = line
		}
	}
	if cause == "" {
		cause = last
	}
	if r := []rune(cause); len(r) > shortErrMax {
		cause = string(r[:shortErrMax-3]) + "..."
	}
	return cause
}

// podsNotReady returns the header and the rows of a `kubectl get pods` table
// for pods that are neither Completed nor Running with every container ready,
// or "" when there are none. A pod that runs without being ready is kept: an
// RGW at 0/2 is exactly the one whose endpoint does not answer.
func podsNotReady(table string) string {
	lines := strings.Split(table, "\n")
	var keep []string
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		switch ready, status := f[1], f[2]; {
		case status == "Completed":
		case status == "Running" && allContainersReady(ready):
		default:
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	return strings.Join(append([]string{lines[0]}, keep...), "\n")
}

// allContainersReady reports whether a READY column, "n/m", counts every
// container ready.
func allContainersReady(ready string) bool {
	n, m, ok := strings.Cut(ready, "/")
	return ok && n == m
}

func init() {
	rootCmd.AddCommand(waitCmd)
	waitCmd.Flags().StringVar(&waitName, "name", "", "kind cluster name")
	waitCmd.Flags().DurationVar(&waitTimeout, "timeout", defaultWaitTimeout, "how long to wait before failing")
}
