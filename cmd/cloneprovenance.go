package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// clonePathFile names the file in a cluster's state directory recording the
// rook clone the cluster was created from.
//
// It exists because liveness alone cannot tell prune's two cases apart. A
// plain 'rooket down' deliberately leaves the state dir, its disk images, and
// its iSCSI targets behind so the next 'up' reuses them without root; a clone
// deleted outright leaves the identical debris with nothing coming back for
// it. Both read as "state dir, no live kind cluster". The clone is what
// separates them, and encodePath is lossy (dash-collapsing, and truncation
// past 45 characters), so the directory's own name cannot be inverted back
// into the path — it has to be recorded.
const clonePathFile = "clone-path"

// recordClonePath notes in a cluster's state dir the rook clone it was created
// from. An empty clone (a --name run outside any rook tree) records nothing,
// and an existing record is never overwritten: the clone that created the
// cluster owns it, so a later command run from a different tree cannot
// silently repoint prune at the wrong directory. Best-effort — a cluster whose
// clone is unrecorded is judged as it was before rooket recorded one.
func recordClonePath(stateDir, clone string) {
	if clone == "" {
		return
	}
	p := filepath.Join(stateDir, clonePathFile)
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.WriteFile(p, []byte(clone+"\n"), 0o644)
}

// cloneDir returns the rook clone a cluster's state dir was created from, or
// "" when nothing records one. The build stamp is a fallback for clusters
// created before rooket wrote clonePathFile; it carries the same path but only
// once a build has run, which is why it cannot be the primary source.
func cloneDir(stateDir string) string {
	if data, err := os.ReadFile(filepath.Join(stateDir, clonePathFile)); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			return p
		}
	}
	data, err := os.ReadFile(filepath.Join(stateDir, buildStampFile))
	if err != nil {
		return ""
	}
	var s struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.Dir
}

// cloneGone reports whether a cluster's state dir is abandoned rather than
// parked: its rook clone is gone, or it names no clone at all. Unknown
// provenance counts as gone so that state dirs predating clonePathFile stay
// sweepable — prune exists for exactly the clone-removed case, and making the
// unknowns immortal would retire it on every host that has one.
func cloneGone(stateDir string) bool {
	clone := cloneDir(stateDir)
	if clone == "" {
		return true
	}
	_, err := os.Stat(clone)
	return err != nil
}

// ownerGone reports whether a cluster's state dir is abandoned rather than
// parked. A cluster deployed from a released Rook is owned by its recorded
// configuration directory and by the clone it was created in, when it has
// them: it is parked while either exists, and always when it has neither,
// since nothing on disk will then disappear to say it was abandoned and its
// record shows it is no leftover from before provenance was recorded. Any
// other cluster is judged by its clone alone (see cloneGone).
func ownerGone(stateDir string) bool {
	src, ok := readSourceAt(stateDir)
	if !ok || src.RookVersion == "" {
		return cloneGone(stateDir)
	}
	owned := false
	for _, owner := range []string{cloneDir(stateDir), src.ConfigDir} {
		if owner == "" {
			continue
		}
		owned = true
		if _, err := os.Stat(owner); err == nil {
			return false
		}
	}
	return owned
}

// parkedBecause says why prune kept a parked cluster.
func parkedBecause(stateDir string) string {
	src, ok := readSourceAt(stateDir)
	if !ok || src.RookVersion == "" {
		return fmt.Sprintf("its clone %s still exists", cloneDir(stateDir))
	}
	if clone := cloneDir(stateDir); clone != "" {
		if _, err := os.Stat(clone); err == nil {
			return fmt.Sprintf("its clone %s still exists", clone)
		}
	}
	if src.ConfigDir != "" {
		if _, err := os.Stat(src.ConfigDir); err == nil {
			return fmt.Sprintf("its configuration directory %s still exists", src.ConfigDir)
		}
	}
	return fmt.Sprintf("it deploys released Rook %s and names no clone or configuration directory", src.RookVersion)
}
