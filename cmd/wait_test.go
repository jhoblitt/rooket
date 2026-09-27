package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	// cephInToolbox is how every ceph command the wait runs starts, bounded so
	// that ceph gives up by itself inside the toolbox.
	cephInToolbox = "-n rook-ceph exec deploy/rook-ceph-tools -- ceph --connect-timeout=20 --rados-mon-op-timeout=20 "

	cephClusterQuery = "-n rook-ceph get cephcluster -o json"
	cephStatusQuery  = cephInToolbox + "status -f json"
	objectStoreQuery = "-n rook-ceph get cephobjectstore -o json"
	rgwEndpoint      = "http://rook-ceph-rgw-ceph-objectstore.rook-ceph.svc:80"

	diagStatusQuery  = cephInToolbox + "status"
	diagHealthQuery  = cephInToolbox + "health detail"
	diagOSDTreeQuery = cephInToolbox + "osd tree"
	diagPodsQuery    = "-n rook-ceph get pods -o wide"
)

// rgwProbeQuery is the RGW probe of endpoint as the stub sees it. kubectl exec
// hands curl each argument as is, so the -w format carries no shell quoting.
func rgwProbeQuery(endpoint string) string {
	return "-n rook-ceph exec deploy/rook-ceph-tools -- curl -s -k --max-time 10 -o /dev/null -w %{http_code} " + endpoint
}

var (
	// errToolboxUnscheduled is kubectl exec against a toolbox pod that exists
	// but is not scheduled yet, as a fresh cluster's often is.
	errToolboxUnscheduled = errors.New("exit status 1: Error from server (BadRequest): " +
		"pod rook-ceph-tools-6d8f9c7b5-x2k4q does not have a host assigned")
	// errCurlNoAnswer is the probe when nothing listens at the endpoint: curl
	// prints 000 and exits 7, and kubectl exec exits with curl's status.
	errCurlNoAnswer = errors.New("exit status 7: command terminated with exit code 7")
)

// cephClusterWithStatus is `kubectl get cephcluster -o json` listing the one
// CephCluster rooket deploys, with the given status.
func cephClusterWithStatus(status string) string {
	return `{"apiVersion":"v1","items":[{"apiVersion":"ceph.rook.io/v1","kind":"CephCluster",` +
		`"metadata":{"name":"rook-ceph","namespace":"rook-ceph"},"spec":{"dataDirHostPath":"/var/lib/rook"},` +
		`"status":` + status + `}],"kind":"List","metadata":{"resourceVersion":""}}`
}

func cephClusterInPhase(phase string) string {
	return cephClusterWithStatus(fmt.Sprintf(`{"phase":%q,"state":"Created",`+
		`"message":"Cluster created successfully","ceph":{"health":"HEALTH_WARN"}}`, phase))
}

const noCephClusters = `{"apiVersion":"v1","items":[],"kind":"List","metadata":{"resourceVersion":""}}`

type pgCount struct {
	state string
	count int
}

// cephStatusJSON is `ceph status -f json` as Squid prints it for a one-mon
// cluster at HEALTH_WARN, with the given OSD counts and PG states, and with
// the fields the wait does not read left in to show they are tolerated.
func cephStatusJSON(osds, up, in int, pgs ...pgCount) string {
	states := make([]string, 0, len(pgs))
	total := 0
	for _, p := range pgs {
		states = append(states, fmt.Sprintf(`{"state_name":%q,"count":%d}`, p.state, p.count))
		total += p.count
	}
	return `{"fsid":"0d6c3d2e-7a5b-4a8f-9e61-3b1f2c4d5e6f",` +
		`"health":{"status":"HEALTH_WARN","checks":{"POOL_NO_REDUNDANCY":{"severity":"HEALTH_WARN",` +
		`"summary":{"message":"3 pool(s) have no replicas configured","count":3},"muted":false}},"mutes":[]},` +
		`"election_epoch":3,"quorum":[0],"quorum_names":["a"],"quorum_age":612,` +
		`"monmap":{"epoch":1,"min_mon_release_name":"squid","num_mons":1},` +
		fmt.Sprintf(`"osdmap":{"epoch":24,"num_osds":%d,"num_up_osds":%d,"osd_up_since":1790000000,`+
			`"num_in_osds":%d,"osd_in_since":1790000000,"num_remapped_pgs":0},`, osds, up, in) +
		fmt.Sprintf(`"pgmap":{"pgs_by_state":[%s],"num_pgs":%d,"num_pools":3,"num_objects":12,`+
			`"data_bytes":590,"bytes_used":82956288,"bytes_avail":32129310720,"bytes_total":32212254720},`,
			strings.Join(states, ","), total) +
		`"fsmap":{"epoch":1,"by_rank":[],"up:standby":0},` +
		`"mgrmap":{"available":true,"num_standbys":0,"modules":["iostat","nfs"],"services":{}},` +
		`"servicemap":{"epoch":2,"modified":"2026-09-27T00:00:00.000000+0000","services":{}},"progress_events":{}}`
}

type storeFixture struct{ name, phase, endpoint string }

// objectStoreList is `kubectl get cephobjectstore -o json` listing a
// CephObjectStore per fixture; one with no endpoint has no status.info.
func objectStoreList(stores ...storeFixture) string {
	items := make([]string, 0, len(stores))
	for _, s := range stores {
		status := fmt.Sprintf(`{"phase":%q}`, s.phase)
		if s.endpoint != "" {
			status = fmt.Sprintf(`{"phase":%q,"info":{"endpoint":%q},"observedGeneration":2}`, s.phase, s.endpoint)
		}
		items = append(items, fmt.Sprintf(`{"apiVersion":"ceph.rook.io/v1","kind":"CephObjectStore",`+
			`"metadata":{"name":%q,"namespace":"rook-ceph"},"spec":{"gateway":{"port":80,"instances":1}},"status":%s}`,
			s.name, status))
	}
	return `{"apiVersion":"v1","items":[` + strings.Join(items, ",") + `],"kind":"List","metadata":{"resourceVersion":""}}`
}

type kubectlAnswer struct {
	out string
	err error
	// block makes the query outlast whatever budget it is given.
	block bool
}

// errBudgetSpent is what the wait's runner returns for a kubectl its
// context's deadline killed.
var errBudgetSpent = fmt.Errorf("%w: signal: killed", context.DeadlineExceeded)

// readyCluster answers every query of one poll for a cluster ready for
// clients: at HEALTH_WARN, with one object store whose RGW answers.
func readyCluster() map[string]kubectlAnswer {
	return map[string]kubectlAnswer{
		cephClusterQuery:           {out: cephClusterInPhase("Ready")},
		cephStatusQuery:            {out: cephStatusJSON(3, 3, 3, pgCount{"active+clean", 49})},
		objectStoreQuery:           {out: objectStoreList(storeFixture{"ceph-objectstore", "Ready", rgwEndpoint})},
		rgwProbeQuery(rgwEndpoint): {out: "200"},
	}
}

// waitHarness runs a readyWaiter against canned polls on a fake clock. Each
// sleep moves on to the next poll's answers, the last poll's answers stand
// for every poll after it, and a query the current poll has no answer for
// fails. The waiter sets its queries' deadlines on the fake clock, so a query
// that blocks moves the clock to its deadline, and one begun at or past its
// deadline fails at once, as the real runner's do.
type waitHarness struct {
	polls []map[string]kubectlAnswer
	now   time.Time
	slept []time.Duration
	calls []string
	out   bytes.Buffer
	diag  bytes.Buffer
}

func newWaitHarness(polls ...map[string]kubectlAnswer) *waitHarness {
	// In the future, so no context the waiter derives from this clock ends in
	// real time: a query here ends by the fake clock alone.
	return &waitHarness{polls: polls, now: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (h *waitHarness) kubectl(ctx context.Context, args ...string) (string, error) {
	line := strings.Join(args, " ")
	h.calls = append(h.calls, line)
	deadline, bounded := ctx.Deadline()
	if bounded && !h.now.Before(deadline) {
		return "", errBudgetSpent
	}
	a, ok := h.polls[min(len(h.slept), len(h.polls)-1)][line]
	if !ok {
		return "", fmt.Errorf("unexpected kubectl %s", line)
	}
	if a.block {
		if !bounded {
			return "", fmt.Errorf("kubectl %s blocked with no deadline", line)
		}
		h.now = deadline
		return "", errBudgetSpent
	}
	return a.out, a.err
}

func (h *waitHarness) waiter() readyWaiter {
	return readyWaiter{
		kubectl:    h.kubectl,
		now:        func() time.Time { return h.now },
		sleep:      func(d time.Duration) { h.slept = append(h.slept, d); h.now = h.now.Add(d) },
		interval:   10 * time.Second,
		pollBudget: time.Minute,
		diagBudget: 30 * time.Second,
		out:        &h.out,
		diag:       &h.diag,
	}
}

// notReadyLines returns the progress lines that name unmet conditions.
func (h *waitHarness) notReadyLines() []string {
	var lines []string
	for line := range strings.Lines(h.out.String()) {
		if strings.Contains(line, "not ready:") {
			lines = append(lines, line)
		}
	}
	return lines
}

func (h *waitHarness) count(query string) int {
	n := 0
	for _, c := range h.calls {
		if c == query {
			n++
		}
	}
	return n
}

func TestWaitReadyOnTheFirstPoll(t *testing.T) {
	h := newWaitHarness(readyCluster())

	if err := h.waiter().wait("alpha", 20*time.Minute); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(h.slept) != 0 {
		t.Errorf("slept %v, want no sleep for a cluster ready on the first poll", h.slept)
	}
	if h.count(rgwProbeQuery(rgwEndpoint)) != 1 {
		t.Errorf("ran %q, want the RGW probed once", h.calls)
	}
	if !strings.Contains(h.out.String(), `cluster "alpha" is ready for clients`) {
		t.Errorf("progress =\n%s\nwant a line saying the cluster is ready", h.out.String())
	}
	if lines := h.notReadyLines(); len(lines) != 0 {
		t.Errorf("printed %q for a cluster that was ready", lines)
	}
	if h.diag.Len() != 0 {
		t.Errorf("printed diagnostics %q for a wait that succeeded", h.diag.String())
	}
}

// A line is printed when the unmet conditions change, not on every poll, so
// a harness's log records the cluster's progress without repeating itself.
func TestWaitBecomesReadyAfterUnmetPolls(t *testing.T) {
	settling := map[string]kubectlAnswer{
		cephClusterQuery: {out: cephClusterInPhase("Progressing")},
		cephStatusQuery:  {out: cephStatusJSON(3, 2, 3, pgCount{"active+clean", 40}, pgCount{"peering", 9})},
		objectStoreQuery: {out: objectStoreList(storeFixture{"ceph-objectstore", "Progressing", ""})},
	}
	almost := readyCluster()
	almost[cephStatusQuery] = kubectlAnswer{out: cephStatusJSON(3, 3, 3, pgCount{"active+clean", 45}, pgCount{"activating", 4})}
	almost[rgwProbeQuery(rgwEndpoint)] = kubectlAnswer{out: "000", err: errCurlNoAnswer}
	h := newWaitHarness(settling, settling, almost, readyCluster())

	if err := h.waiter().wait("alpha", 20*time.Minute); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if want := []time.Duration{10 * time.Second, 10 * time.Second, 10 * time.Second}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
	lines := h.notReadyLines()
	if len(lines) != 2 {
		t.Fatalf("progress =\n%s\nwant one not-ready line per change: 2 for 3 unmet polls, two of them alike", h.out.String())
	}
	for i, want := range [][]string{
		{`CephCluster phase is "Progressing", not Ready`, "OSDs: 2 up and 3 in of 3", "PGs: 40 of 49 active+clean",
			`object store "ceph-objectstore" phase is "Progressing", not Ready`},
		{"PGs: 45 of 49 active+clean", `object store "ceph-objectstore": RGW at ` + rgwEndpoint + " did not answer"},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("not-ready line %d = %q, want it to name %q", i, lines[i], w)
			}
		}
	}
	if strings.Contains(lines[1], "CephCluster") || strings.Contains(lines[1], "OSDs") {
		t.Errorf("not-ready line = %q, names conditions met by then", lines[1])
	}
	if !strings.Contains(h.out.String(), `cluster "alpha" is ready for clients`) {
		t.Errorf("progress =\n%s\nwant a line saying the cluster is ready", h.out.String())
	}
	if h.diag.Len() != 0 {
		t.Errorf("printed diagnostics %q for a wait that succeeded", h.diag.String())
	}
}

// A fresh cluster's toolbox pod often exists before it is scheduled, and
// every exec into it fails until it is: that is a condition still unmet, not
// a reason to give up.
func TestWaitKeepsPollingThroughAFailedQuery(t *testing.T) {
	unscheduled := readyCluster()
	unscheduled[cephStatusQuery] = kubectlAnswer{err: errToolboxUnscheduled}
	unscheduled[rgwProbeQuery(rgwEndpoint)] = kubectlAnswer{err: errToolboxUnscheduled}
	h := newWaitHarness(unscheduled, readyCluster())

	if err := h.waiter().wait("alpha", 20*time.Minute); err != nil {
		t.Fatalf("wait: %v, want it to keep polling past a failed query", err)
	}
	if len(h.slept) != 1 {
		t.Errorf("slept %v, want one interval before the ready poll", h.slept)
	}
	lines := h.notReadyLines()
	if len(lines) != 1 {
		t.Fatalf("progress =\n%s\nwant one not-ready line", h.out.String())
	}
	short := "Error from server (BadRequest): pod rook-ceph-tools-6d8f9c7b5-x2k4q does not have a host assigned"
	if !strings.Contains(lines[0], "ceph status: "+short) {
		t.Errorf("not-ready line = %q, want the ceph status failure named as %q", lines[0], short)
	}
	if strings.Contains(lines[0], "exit status") {
		t.Errorf("not-ready line = %q, want the failure in short form", lines[0])
	}
}

// podsTable is `kubectl -n rook-ceph get pods -o wide` partway to ready.
const podsTable = `NAME                                           READY   STATUS             RESTARTS     AGE   IP           NODE            NOMINATED NODE   READINESS GATES
rook-ceph-mon-a-6b7d9c8f5-abcde                2/2     Running            0            20m   10.89.0.5    alpha-worker    <none>           <none>
rook-ceph-osd-1-7c9d8b6f4-fghij                1/2     CrashLoopBackOff   6 (2m ago)   18m   10.89.0.6    alpha-worker2   <none>           <none>
rook-ceph-osd-prepare-alpha-worker-xyz12       0/1     Completed          0            19m   10.244.1.7   alpha-worker    <none>           <none>
rook-ceph-rgw-ceph-objectstore-a-5f6d7-klmno   0/2     Running            0            15m   10.89.0.5    alpha-worker    <none>           <none>
rook-ceph-tools-6d8f9c7b5-x2k4q                0/1     Pending            0            20m   <none>       <none>          <none>           <none>`

func TestWaitTimesOutNamingTheUnmetConditions(t *testing.T) {
	stuck := readyCluster()
	stuck[cephStatusQuery] = kubectlAnswer{out: cephStatusJSON(3, 2, 3,
		pgCount{"active+clean", 45}, pgCount{"active+undersized+degraded", 4})}
	stuck[rgwProbeQuery(rgwEndpoint)] = kubectlAnswer{out: "000", err: errCurlNoAnswer}
	stuck[diagStatusQuery] = kubectlAnswer{out: "  cluster:\n    id:     0d6c3d2e-7a5b-4a8f-9e61-3b1f2c4d5e6f\n" +
		"    health: HEALTH_WARN\n            1 osds down"}
	stuck[diagHealthQuery] = kubectlAnswer{err: errors.New(`exit status 1: error: unable to upgrade connection: container not found ("rook-ceph-tools")`)}
	stuck[diagOSDTreeQuery] = kubectlAnswer{out: "ID  CLASS  WEIGHT   TYPE NAME               STATUS  REWEIGHT  PRI-AFF\n" +
		" 1    hdd  0.00980          osd.1             down   1.00000  1.00000"}
	stuck[diagPodsQuery] = kubectlAnswer{out: podsTable}
	h := newWaitHarness(stuck)

	err := h.waiter().wait("alpha", time.Minute)
	if err == nil {
		t.Fatal("wait succeeded for a cluster that never became ready")
	}
	for _, want := range []string{`"alpha"`, "1m0s", "OSDs: 2 up and 3 in of 3", "PGs: 45 of 49 active+clean",
		`object store "ceph-objectstore": RGW at ` + rgwEndpoint + " did not answer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wait error = %q, want it to name %q", err, want)
		}
	}
	var slept time.Duration
	for _, d := range h.slept {
		slept += d
	}
	if slept != time.Minute {
		t.Errorf("slept %v in all, want the whole timeout, %v", slept, time.Minute)
	}
	diag := h.diag.String()
	for _, want := range []string{
		"ceph status", "1 osds down",
		"ceph health detail", `container not found ("rook-ceph-tools")`,
		"ceph osd tree", "osd.1",
		"rook-ceph-osd-1-7c9d8b6f4-fghij", "rook-ceph-rgw-ceph-objectstore-a-5f6d7-klmno", "rook-ceph-tools-6d8f9c7b5-x2k4q",
	} {
		if !strings.Contains(diag, want) {
			t.Errorf("diagnostics =\n%s\nwant them to include %q", diag, want)
		}
	}
	for _, healthy := range []string{"rook-ceph-mon-a-6b7d9c8f5-abcde", "rook-ceph-osd-prepare-alpha-worker-xyz12"} {
		if strings.Contains(diag, healthy) {
			t.Errorf("diagnostics =\n%s\nlist %s, which is ready or Completed", diag, healthy)
		}
	}
	if h.out.Len() == 0 || strings.Contains(h.out.String(), "osd.1") {
		t.Errorf("progress =\n%s\nwant progress lines there and the diagnostics kept apart from them", h.out.String())
	}
}

// The last sleep is cut short at the deadline, so the final poll lands on it
// rather than an interval past it.
func TestWaitNeverSleepsPastTheTimeout(t *testing.T) {
	stuck := readyCluster()
	stuck[cephClusterQuery] = kubectlAnswer{out: cephClusterInPhase("Progressing")}
	h := newWaitHarness(stuck)

	if err := h.waiter().wait("alpha", 25*time.Second); err == nil {
		t.Fatal("wait succeeded for a cluster that never became ready")
	}
	if want := []time.Duration{10 * time.Second, 10 * time.Second, 5 * time.Second}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
	if n := h.count(cephClusterQuery); n != 4 {
		t.Errorf("polled %d times, want 4: at 0s, 10s, 20s, and the 25s deadline", n)
	}
}

// A query that outlasts the poll's budget is cut off and named as unmet, and
// so is every query the spent budget leaves no time for; the next poll starts
// with a fresh budget.
func TestWaitProceedsPastAQueryThatOutlastsItsBudget(t *testing.T) {
	hung := readyCluster()
	hung[cephStatusQuery] = kubectlAnswer{block: true}
	h := newWaitHarness(hung, readyCluster())

	if err := h.waiter().wait("alpha", 20*time.Minute); err != nil {
		t.Fatalf("wait: %v, want it to carry on past a query cut off by its budget", err)
	}
	if want := []time.Duration{10 * time.Second}; !slices.Equal(h.slept, want) {
		t.Errorf("slept %v, want %v", h.slept, want)
	}
	lines := h.notReadyLines()
	if len(lines) != 1 {
		t.Fatalf("progress =\n%s\nwant one not-ready line", h.out.String())
	}
	for _, want := range []string{
		"ceph status: timed out: the poll's 1m0s budget ran out",
		"object stores: timed out: the poll's 1m0s budget ran out",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("not-ready line = %q, want it to name %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "signal: killed") {
		t.Errorf("not-ready line = %q, want the budget named rather than the kill", lines[0])
	}
}

// --timeout is checked between polls, so a poll that hangs overruns it by at
// most its own budget, and each diagnostic after it by at most its own.
func TestWaitEndsWithinOnePollBudgetOfItsTimeout(t *testing.T) {
	hung := readyCluster()
	hung[cephStatusQuery] = kubectlAnswer{block: true}
	hung[diagStatusQuery] = kubectlAnswer{block: true}
	hung[diagHealthQuery] = kubectlAnswer{out: "HEALTH_WARN 1 osds down"}
	hung[diagOSDTreeQuery] = kubectlAnswer{out: "ID  CLASS  WEIGHT"}
	hung[diagPodsQuery] = kubectlAnswer{out: podsTable}
	h := newWaitHarness(hung)
	w := h.waiter()
	start := h.now

	err := w.wait("alpha", 25*time.Second)
	if err == nil {
		t.Fatal("wait succeeded for a cluster that never became ready")
	}
	if n := h.count(cephClusterQuery); n != 1 {
		t.Errorf("polled %d times, want 1: the one poll outlasted the timeout", n)
	}
	for _, want := range []string{"after 1m0s", "timeout 25s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wait error = %q, want it to report %q: the time taken, not only the timeout", err, want)
		}
	}
	if took, most := h.now.Sub(start), 25*time.Second+w.pollBudget+4*w.diagBudget; took > most {
		t.Errorf("took %v, want at most the timeout plus one poll's and four diagnostics' budgets, %v", took, most)
	}
	diag := h.diag.String()
	for _, want := range []string{"timed out: its 30s budget ran out", "HEALTH_WARN 1 osds down", "ID  CLASS  WEIGHT"} {
		if !strings.Contains(diag, want) {
			t.Errorf("diagnostics =\n%s\nwant them to include %q", diag, want)
		}
	}
}

// Killing kubectl leaves what it started in the toolbox running, so each
// command there carries its own bound and gives up by itself.
func TestWaitBoundsWhatRunsInTheToolbox(t *testing.T) {
	stuck := readyCluster()
	stuck[cephStatusQuery] = kubectlAnswer{out: cephStatusJSON(3, 2, 3, pgCount{"active+clean", 49})}
	stuck[diagStatusQuery] = kubectlAnswer{out: "HEALTH_WARN"}
	stuck[diagHealthQuery] = kubectlAnswer{out: "HEALTH_WARN"}
	stuck[diagOSDTreeQuery] = kubectlAnswer{out: "ID"}
	stuck[diagPodsQuery] = kubectlAnswer{out: podsTable}
	h := newWaitHarness(stuck)

	if err := h.waiter().wait("alpha", 10*time.Second); err == nil {
		t.Fatal("wait succeeded for a cluster that never became ready")
	}
	for _, c := range h.calls {
		_, command, inToolbox := strings.Cut(c, " -- ")
		switch {
		case !inToolbox:
		case strings.HasPrefix(command, "ceph "):
			if !strings.HasPrefix(command, "ceph --connect-timeout=20 --rados-mon-op-timeout=20 ") {
				t.Errorf("ran %q, want ceph bounded by --connect-timeout and --rados-mon-op-timeout", command)
			}
		case strings.HasPrefix(command, "curl "):
			if !strings.Contains(command, " --max-time 10 ") || !strings.Contains(command, " -k ") {
				t.Errorf("ran %q, want curl bounded by --max-time and accepting the RGW's certificate", command)
			}
		default:
			t.Errorf("ran %q in the toolbox, which this test does not know to check", command)
		}
	}
}

// Only a store that is Ready is probed: until then, its phase already says
// what is unmet.
func TestWaitProbesOnlyAReadyObjectStore(t *testing.T) {
	provisioning := readyCluster()
	provisioning[objectStoreQuery] = kubectlAnswer{out: objectStoreList(storeFixture{"ceph-objectstore", "Progressing", rgwEndpoint})}
	h := newWaitHarness(provisioning, readyCluster())

	if err := h.waiter().wait("alpha", 20*time.Minute); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if n := h.count(rgwProbeQuery(rgwEndpoint)); n != 1 {
		t.Errorf("probed the RGW %d times, want once: on the poll that found the store Ready", n)
	}
}

// A PG is ready when it is active and clean and its IO is not paused: a scrub,
// a remap, a snap trim or a repair goes on while the PG keeps serving
// clients, but a stale, laggy, waiting or premerge PG does not serve. States
// are compared token by token, so no prefix or substring passes for one.
func TestPGActiveClean(t *testing.T) {
	for state, want := range map[string]bool{
		"active+clean":                                    true,
		"active+clean+scrubbing":                          true,
		"active+clean+scrubbing+deep":                     true,
		"active+clean+remapped":                           true,
		"active+clean+snaptrim":                           true,
		"active+clean+snaptrim_wait":                      true,
		"active+clean+inconsistent":                       true,
		"active+clean+scrubbing+deep+inconsistent+repair": true,
		"clean+active":                                    true,
		"stale+active+clean":                              false,
		"active+clean+laggy":                              false,
		"active+clean+wait":                               false,
		"active+clean+premerge":                           false,
		"active+undersized+degraded":                      false,
		"active+recovery_wait+degraded":                   false,
		"peering":                                         false,
		"unknown":                                         false,
		"activating":                                      false,
		"creating+peering":                                false,
		"active":                                          false,
		"active+cleaning":                                 false,
		"inactive+clean":                                  false,
		"":                                                false,
	} {
		if got := pgActiveClean(state); got != want {
			t.Errorf("pgActiveClean(%q) = %v, want %v", state, got, want)
		}
	}
}

// HEALTH_OK is never asked for: every fixture here is at HEALTH_WARN, as a
// single-worker cluster settles.
func TestCephStatusUnmet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   []string
	}{
		{name: "ready", status: cephStatusJSON(3, 3, 3, pgCount{"active+clean", 49})},
		{
			name: "scrubbing PGs are ready",
			status: cephStatusJSON(3, 3, 3, pgCount{"active+clean", 30},
				pgCount{"active+clean+scrubbing", 12}, pgCount{"active+clean+scrubbing+deep", 7}),
		},
		{name: "zero OSDs", status: cephStatusJSON(0, 0, 0, pgCount{"unknown", 1}), want: []string{"no OSDs", "PGs: 0 of 1 active+clean"}},
		{name: "an OSD down", status: cephStatusJSON(3, 2, 3, pgCount{"active+clean", 49}), want: []string{"OSDs: 2 up and 3 in of 3"}},
		{name: "an OSD out", status: cephStatusJSON(3, 3, 2, pgCount{"active+clean", 49}), want: []string{"OSDs: 3 up and 2 in of 3"}},
		{name: "no PGs", status: cephStatusJSON(3, 3, 3), want: []string{"no PGs"}},
		{
			name:   "degraded PGs",
			status: cephStatusJSON(3, 3, 3, pgCount{"active+clean", 45}, pgCount{"active+undersized+degraded", 4}),
			want:   []string{"PGs: 45 of 49 active+clean"},
		},
		{
			name:   "peering and unknown PGs",
			status: cephStatusJSON(3, 3, 3, pgCount{"peering", 20}, pgCount{"unknown", 29}),
			want:   []string{"PGs: 0 of 49 active+clean"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := parseCephStatus(tc.status)
			if err != nil {
				t.Fatalf("parseCephStatus: %v", err)
			}
			if got := cephStatusUnmet(st); !slices.Equal(got, tc.want) {
				t.Errorf("cephStatusUnmet() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseCephStatus(t *testing.T) {
	st, err := parseCephStatus(cephStatusJSON(3, 2, 1, pgCount{"active+clean", 40}, pgCount{"peering", 9}))
	if err != nil {
		t.Fatalf("parseCephStatus: %v", err)
	}
	if o := st.OSDMap; o.NumOSDs != 3 || o.NumUpOSDs != 2 || o.NumInOSDs != 1 {
		t.Errorf("osdmap = %+v, want 3 OSDs, 2 up, 1 in", o)
	}
	if p := st.PGMap; p.NumPGs != 49 || len(p.PGsByState) != 2 || p.PGsByState[1].StateName != "peering" || p.PGsByState[1].Count != 9 {
		t.Errorf("pgmap = %+v, want 49 PGs in two states", p)
	}
	if _, err := parseCephStatus("Error initializing cluster client"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("parseCephStatus(not JSON) = %v, want a parse error", err)
	}
}

func TestParseCephClusterPhase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		list    string
		want    string
		wantErr error
	}{
		{name: "Ready", list: cephClusterInPhase("Ready"), want: "Ready"},
		{name: "Progressing", list: cephClusterInPhase("Progressing"), want: "Progressing"},
		{name: "no status yet", list: cephClusterWithStatus(`{}`), want: ""},
		{
			name: "the first of several decides",
			list: `{"items":[{"status":{"phase":"Progressing"}},{"status":{"phase":"Ready"}}]}`,
			want: "Progressing",
		},
		{name: "no CephCluster", list: noCephClusters, wantErr: errNoCephCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCephClusterPhase(tc.list)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Errorf("parseCephClusterPhase() = %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := parseCephClusterPhase(`error: the server doesn't have a resource type "cephcluster"`); err == nil ||
		!strings.Contains(err.Error(), "parse") {
		t.Errorf("parseCephClusterPhase(not JSON) = %v, want a parse error", err)
	}
}

func TestParseObjectStores(t *testing.T) {
	got, err := parseObjectStores(objectStoreList(
		storeFixture{"ceph-objectstore", "Ready", rgwEndpoint},
		storeFixture{"archive", "Progressing", ""},
	))
	if err != nil {
		t.Fatalf("parseObjectStores: %v", err)
	}
	want := []objectStore{
		{name: "ceph-objectstore", phase: "Ready", endpoint: rgwEndpoint},
		{name: "archive", phase: "Progressing"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseObjectStores() = %+v, want %+v", got, want)
	}
	if got, err := parseObjectStores(objectStoreList()); err != nil || len(got) != 0 {
		t.Errorf("parseObjectStores(no stores) = %+v, %v; want none", got, err)
	}
	if _, err := parseObjectStores("No resources found"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("parseObjectStores(not JSON) = %v, want a parse error", err)
	}
}

// readyReading is one poll's reading of a cluster ready for clients, with no
// object store.
func readyReading(t *testing.T) readiness {
	t.Helper()
	st, err := parseCephStatus(cephStatusJSON(3, 3, 3, pgCount{"active+clean", 49}))
	if err != nil {
		t.Fatal(err)
	}
	return readiness{phase: "Ready", status: st}
}

func TestUnmetConditions(t *testing.T) {
	readyStore := objectStore{name: "s3", phase: "Ready", endpoint: rgwEndpoint}
	for _, tc := range []struct {
		name   string
		change func(*readiness)
		want   []string
	}{
		{name: "no object stores is fine", change: func(*readiness) {}},
		{
			name: "an RGW that answers",
			change: func(r *readiness) {
				r.stores, r.rgw = []objectStore{readyStore}, map[string]rgwProbe{"s3": {code: "200"}}
			},
		},
		{
			name: "an RGW that answers with an error status still answers",
			change: func(r *readiness) {
				r.stores, r.rgw = []objectStore{readyStore}, map[string]rgwProbe{"s3": {code: "503"}}
			},
		},
		{
			name: "an RGW that does not answer",
			change: func(r *readiness) {
				r.stores, r.rgw = []objectStore{readyStore}, map[string]rgwProbe{"s3": {code: "000"}}
			},
			want: []string{`object store "s3": RGW at ` + rgwEndpoint + " did not answer (HTTP 000)"},
		},
		{
			name: "an RGW curl cannot connect to",
			change: func(r *readiness) {
				r.stores, r.rgw = []objectStore{readyStore}, map[string]rgwProbe{"s3": {code: "000", err: errCurlNoAnswer}}
			},
			want: []string{`object store "s3": RGW at ` + rgwEndpoint + " did not answer (HTTP 000; command terminated with exit code 7)"},
		},
		{
			name: "an RGW probe the toolbox cannot run",
			change: func(r *readiness) {
				r.stores, r.rgw = []objectStore{readyStore}, map[string]rgwProbe{"s3": {err: errToolboxUnscheduled}}
			},
			want: []string{`object store "s3": RGW at ` + rgwEndpoint + " did not answer (Error from server (BadRequest): " +
				"pod rook-ceph-tools-6d8f9c7b5-x2k4q does not have a host assigned)"},
		},
		{
			name: "an object store not Ready",
			change: func(r *readiness) {
				r.stores = []objectStore{{name: "s3", phase: "Progressing", endpoint: rgwEndpoint}}
			},
			want: []string{`object store "s3" phase is "Progressing", not Ready`},
		},
		{
			name:   "an object store with no phase yet",
			change: func(r *readiness) { r.stores = []objectStore{{name: "s3"}} },
			want:   []string{`object store "s3" has no phase yet`},
		},
		{
			name:   "a Ready object store with no endpoint",
			change: func(r *readiness) { r.stores = []objectStore{{name: "s3", phase: "Ready"}} },
			want:   []string{`object store "s3" has no endpoint yet`},
		},
		{
			name: "the object stores cannot be listed",
			change: func(r *readiness) {
				r.storesErr = errors.New(`exit status 1: error: the server doesn't have a resource type "cephobjectstore"`)
			},
			want: []string{`object stores: error: the server doesn't have a resource type "cephobjectstore"`},
		},
		{
			name:   "CephCluster not Ready",
			change: func(r *readiness) { r.phase = "Progressing" },
			want:   []string{`CephCluster phase is "Progressing", not Ready`},
		},
		{
			name:   "CephCluster with no phase yet",
			change: func(r *readiness) { r.phase = "" },
			want:   []string{"CephCluster has no phase yet"},
		},
		{
			name:   "no CephCluster",
			change: func(r *readiness) { r.phase, r.phaseErr = "", errNoCephCluster },
			want:   []string{"no CephCluster in rook-ceph"},
		},
		{
			name: "the CephCluster cannot be read",
			change: func(r *readiness) {
				r.phase, r.phaseErr = "", errors.New("exit status 1: The connection to the server 127.0.0.1:6443 was refused")
			},
			want: []string{"CephCluster: The connection to the server 127.0.0.1:6443 was refused"},
		},
		{
			name:   "ceph status cannot be read",
			change: func(r *readiness) { r.status, r.statusErr = cephStatus{}, errToolboxUnscheduled },
			want: []string{"ceph status: Error from server (BadRequest): " +
				"pod rook-ceph-tools-6d8f9c7b5-x2k4q does not have a host assigned"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readyReading(t)
			tc.change(&r)
			if got := unmetConditions(r); !slices.Equal(got, tc.want) {
				t.Errorf("unmetConditions() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRGWAnswered(t *testing.T) {
	for code, want := range map[string]bool{
		"100": true, "200": true, "301": true, "403": true, "404": true, "503": true, "599": true,
		"000": false, "099": false, "600": false, "2000": false, "20": false, "+20": false, "abc": false, "": false,
	} {
		if got := rgwAnswered(code); got != want {
			t.Errorf("rgwAnswered(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestShortErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "kubectl's exit status is dropped",
			err:  errToolboxUnscheduled,
			want: "Error from server (BadRequest): pod rook-ceph-tools-6d8f9c7b5-x2k4q does not have a host assigned",
		},
		{
			name: "the last line that names the failure, not kubectl's exit code",
			err: errors.New("exit status 1: 2026-09-27T12:00:00.000+0000 7f3a monclient(hunting): authenticate timed out after 300\n" +
				"[errno 110] RADOS timed out (error connecting to the cluster)\ncommand terminated with exit code 1\n"),
			want: "[errno 110] RADOS timed out (error connecting to the cluster)",
		},
		{name: "kubectl's exit code when nothing else says why", err: errCurlNoAnswer, want: "command terminated with exit code 7"},
		{name: "no exit status", err: errors.New("unexpected kubectl get pods"), want: "unexpected kubectl get pods"},
		{name: "a long line is cut", err: errors.New(strings.Repeat("x", 300)), want: strings.Repeat("x", 157) + "..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortErr(tc.err); got != tc.want {
				t.Errorf("shortErr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A pod that runs without being ready is listed with those that do not run:
// an RGW at 0/2 is exactly the one whose endpoint does not answer.
func TestPodsNotReady(t *testing.T) {
	got := podsNotReady(podsTable)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAME ") ||
		!strings.HasPrefix(lines[1], "rook-ceph-osd-1-7c9d8b6f4-fghij ") ||
		!strings.HasPrefix(lines[2], "rook-ceph-rgw-ceph-objectstore-a-5f6d7-klmno ") ||
		!strings.HasPrefix(lines[3], "rook-ceph-tools-6d8f9c7b5-x2k4q ") {
		t.Errorf("podsNotReady() =\n%s\nwant the header, the crash-looping OSD, the running but unready RGW, "+
			"and the pending toolbox", got)
	}
	healthy := strings.Join(slices.DeleteFunc(strings.Split(podsTable, "\n"), func(l string) bool {
		return strings.Contains(l, "CrashLoopBackOff") || strings.Contains(l, "Pending") || strings.Contains(l, "0/2 ")
	}), "\n")
	if got := podsNotReady(healthy); got != "" {
		t.Errorf("podsNotReady(every pod ready or Completed) = %q, want nothing", got)
	}
	if got := podsNotReady(""); got != "" {
		t.Errorf("podsNotReady(no pods) = %q, want nothing", got)
	}
}

// stubWaitKubectl answers waitKubectl from h for the test, recording the
// $KUBECONFIG each query ran with. It stops the test at a query whose context
// carries no deadline within a poll's budget, so a wait that lost its budgets
// fails. A wait run through waitKubectl keeps real time, not h's, so its
// queries' real deadlines are then hidden from h, which would read them on its
// own clock.
func stubWaitKubectl(t *testing.T, h *waitHarness) *[]string {
	t.Helper()
	prev := waitKubectl
	var seen []string
	waitKubectl = func(ctx context.Context, args ...string) (string, error) {
		seen = append(seen, os.Getenv("KUBECONFIG"))
		now := time.Now()
		// Fatal rather than an error, which would leave the wait polling on
		// real time until its timeout.
		if deadline, ok := ctx.Deadline(); !ok || !deadline.After(now) || deadline.After(now.Add(waitPollBudget)) {
			t.Fatalf("kubectl %s ran with deadline %v (set: %v), want one within %s from now",
				strings.Join(args, " "), deadline, ok, waitPollBudget)
		}
		return h.kubectl(context.WithoutCancel(ctx), args...)
	}
	t.Cleanup(func() { waitKubectl = prev })
	return &seen
}

// The wait's queries repeat every poll: traced as ceph-config's are, they
// would bury the lines saying what the wait is still waiting for.
func TestWaitKubectlIsUntraced(t *testing.T) {
	fakeKubectl(t)

	var (
		got string
		err error
	)
	stdout := captureStdout(t, func() { got, err = waitKubectl(context.Background(), "get", "cephcluster") })
	if err != nil || got != "stdout: get cephcluster" {
		t.Fatalf("waitKubectl() = %q, %v; want kubectl's trimmed stdout", got, err)
	}
	if stdout != "" {
		t.Errorf("waitKubectl printed %q, want nothing", stdout)
	}
	if _, err := waitKubectl(context.Background(), "fail", "exec"); err == nil || !strings.Contains(err.Error(), "stderr: fail exec") {
		t.Errorf("waitKubectl() error = %v, want kubectl's stderr in it", err)
	}
}

// A deadline that has passed stops the real runner before it starts kubectl,
// and the error says so, which is what the wait's budgets rest on.
func TestWaitKubectlHonorsItsDeadline(t *testing.T) {
	fakeKubectl(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if out, err := waitKubectl(ctx, "get", "cephcluster"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waitKubectl() past its deadline = %q, %v; want context.DeadlineExceeded", out, err)
	}
}

// captureStdout returns what fn writes to os.Stdout, where run.Printf and
// rooket's progress lines go.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = prev })
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func setWaitFlags(t *testing.T, name string) {
	t.Helper()
	prevName, prevTimeout := waitName, waitTimeout
	waitName, waitTimeout = name, 20*time.Minute
	t.Cleanup(func() { waitName, waitTimeout = prevName, prevTimeout })
}

func TestWaitCmdTargetsTheSelectedCluster(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env, want string
	}{
		{name: "--name", flag: "alpha", env: "beta", want: "alpha"},
		{name: "ROOKET_NAME", env: "beta", want: "beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ROOKET_NAME", tc.env)
			t.Setenv("KUBECONFIG", "")
			setWaitFlags(t, tc.flag)
			kc := writeKubeconfig(t, tc.want)
			seen := stubWaitKubectl(t, newWaitHarness(readyCluster()))

			var runErr error
			out := captureStdout(t, func() { runErr = waitCmd.RunE(waitCmd, nil) })
			if runErr != nil {
				t.Fatalf("wait: %v", runErr)
			}
			if len(*seen) == 0 {
				t.Fatal("ran no kubectl")
			}
			for _, got := range *seen {
				if got != kc {
					t.Errorf("kubectl ran with KUBECONFIG=%q, want %q", got, kc)
				}
			}
			if !strings.Contains(out, fmt.Sprintf("cluster %q is ready for clients", tc.want)) {
				t.Errorf("stdout =\n%s\nwant the ready line on stdout, where progress goes", out)
			}
		})
	}
}

func TestWaitCmdNeedsTheClusterUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	setWaitFlags(t, "alpha")
	h := newWaitHarness(readyCluster())
	stubWaitKubectl(t, h)

	err := waitCmd.RunE(waitCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "is it up?") {
		t.Fatalf("wait without a kubeconfig = %v, want an is-it-up error", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("ran %q against a cluster with no kubeconfig", h.calls)
	}
}

// A timeout that is not positive would poll once and fail; it is refused
// before anything is resolved or queried.
func TestWaitCmdRefusesATimeoutThatIsNotPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Minute} {
		t.Run(d.String(), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("KUBECONFIG", "untouched")
			setWaitFlags(t, "alpha")
			waitTimeout = d
			writeKubeconfig(t, "alpha")
			h := newWaitHarness(readyCluster())
			stubWaitKubectl(t, h)

			err := waitCmd.RunE(waitCmd, nil)
			if err == nil || !strings.Contains(err.Error(), "--timeout must be more than 0") {
				t.Fatalf("wait --timeout %s = %v, want it refused, naming --timeout", d, err)
			}
			if len(h.calls) != 0 {
				t.Errorf("ran %q for a refused --timeout", h.calls)
			}
			if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
				t.Errorf("KUBECONFIG = %q: the cluster was resolved before --timeout was checked", kc)
			}
		})
	}
}

func TestCheckUpWaitFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wait       bool
		timeoutSet bool
		timeout    time.Duration
		wantErr    string
	}{
		{name: "neither", timeout: defaultWaitTimeout},
		{name: "--wait alone", wait: true, timeout: defaultWaitTimeout},
		{name: "--wait with a --wait-timeout", wait: true, timeoutSet: true, timeout: 5 * time.Minute},
		{name: "--wait-timeout without --wait", timeoutSet: true, timeout: 5 * time.Minute, wantErr: "--wait-timeout needs --wait"},
		{name: "a --wait-timeout of zero", wait: true, timeoutSet: true, timeout: 0, wantErr: "--wait-timeout must be more than 0"},
		{name: "a negative --wait-timeout", wait: true, timeoutSet: true, timeout: -time.Minute, wantErr: "--wait-timeout must be more than 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUpWaitFlags(tc.wait, tc.timeoutSet, tc.timeout)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkUpWaitFlags() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkUpWaitFlags() = %v, want an error naming %s", err, tc.wantErr)
			}
		})
	}
}

// up refuses a --wait-timeout it would ignore, or one that is not positive,
// before it resolves the cluster, let alone stands anything up.
func TestUpRunERefusesAWaitTimeoutItCannotUse(t *testing.T) {
	// The subtests are not named after the flags: t.TempDir puts a test's
	// name in the paths an unrelated error would quote.
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "stray", args: []string{"--wait-timeout=5m"}, want: "--wait-timeout needs --wait"},
		{name: "zero", args: []string{"--wait", "--wait-timeout=0s"}, want: "--wait-timeout must be more than 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateUpSource(t)
			t.Setenv("KUBECONFIG", "untouched")
			name := upName
			t.Cleanup(func() { upName = name })
			parseUpFlags(t, tc.args...)

			err := upCmd.RunE(upCmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("up %s = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
			}
			if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
				t.Errorf("KUBECONFIG = %q: up resolved the cluster before refusing its flags", kc)
			}
		})
	}
}

func TestWaitFlagDefaults(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		flag string
		want string
	}{
		{"wait", "timeout", "20m0s"},
		{"up", "wait-timeout", "20m0s"},
		{"up", "wait", "false"},
	} {
		c := waitCmd
		if tc.cmd == "up" {
			c = upCmd
		}
		f := c.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("%s has no --%s", tc.cmd, tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("%s --%s defaults to %q, want %q", tc.cmd, tc.flag, f.DefValue, tc.want)
		}
	}
}

func TestUpWaitStepSkippedWithSkipDeploy(t *testing.T) {
	h := newWaitHarness(readyCluster())
	stubWaitKubectl(t, h)

	var err error
	out := captureStdout(t, func() { err = upWaitStep("alpha", true, 20*time.Minute, &bytes.Buffer{}) })
	if err != nil {
		t.Fatalf("upWaitStep: %v", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("ran %q with --skip-deploy", h.calls)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 ||
		!strings.Contains(lines[0], "skipped") || !strings.Contains(lines[0], "--skip-deploy") {
		t.Errorf("stdout = %q, want one line saying --skip-deploy skipped the wait", out)
	}
}

func TestUpWaitStepWaitsOnTheCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")
	h := newWaitHarness(readyCluster())
	stubWaitKubectl(t, h)

	var (
		diag bytes.Buffer
		err  error
	)
	captureStdout(t, func() { err = upWaitStep("alpha", false, 20*time.Minute, &diag) })
	if err == nil || !strings.Contains(err.Error(), "is it up?") {
		t.Fatalf("upWaitStep with no kubeconfig = %v, want an is-it-up error", err)
	}
	if len(h.calls) != 0 {
		t.Errorf("ran %q against a cluster with no kubeconfig", h.calls)
	}

	writeKubeconfig(t, "alpha")
	out := captureStdout(t, func() { err = upWaitStep("alpha", false, 20*time.Minute, &diag) })
	if err != nil {
		t.Fatalf("upWaitStep: %v", err)
	}
	if h.count(cephStatusQuery) != 1 {
		t.Errorf("ran %q, want one poll of the cluster", h.calls)
	}
	if !strings.Contains(out, `cluster "alpha" is ready for clients`) {
		t.Errorf("stdout =\n%s\nwant the ready line", out)
	}
}
