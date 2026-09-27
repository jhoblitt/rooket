package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// shapeFile names the record, in a cluster's state directory, of the shape the
// cluster was created with.
//
// deploy pins one OSD device per worker and block teardown names each worker's
// iSCSI targets, so both need the worker count, disk count, and IQN date the
// cluster was built with. Their flags default to three workers: without a
// record, a cluster brought up with --workers 1 had every later deploy wait on
// disks that were never created.
const shapeFile = "shape.json"

// clusterShape is what 'rooket cluster create' built a cluster with.
type clusterShape struct {
	Workers   int    `json:"workers"`
	DiskCount int    `json:"diskCount"`
	IQNDate   string `json:"iqnDate"`
}

// readShape returns the shape recorded for a cluster. A cluster created before
// rooket recorded shapes, or whose record is unreadable, has none.
func readShape(name string) (clusterShape, bool) {
	dir, err := stateDirPath(name)
	if err != nil {
		return clusterShape{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, shapeFile))
	if err != nil {
		return clusterShape{}, false
	}
	var s clusterShape
	if json.Unmarshal(data, &s) != nil {
		return clusterShape{}, false
	}
	return s, true
}

// writeShape records a cluster's shape atomically (temp+rename), so a torn
// write leaves an unreadable record rather than a wrong one.
func writeShape(name string, s clusterShape) error {
	dir, err := ensureStateDir(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, shapeFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("record cluster shape: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("record cluster shape: %w", err)
	}
	return nil
}

// workerCountChanged reports whether a run asks an existing cluster for a
// different number of workers than it was created with, and the recorded count
// when it does.
func workerCountChanged(name string, workers int) (int, bool) {
	rec, ok := readShape(name)
	return rec.Workers, ok && rec.Workers != workers
}

// shapeUse is what a command means by a shape flag the user passed.
type shapeUse int

const (
	// matchShape is for commands that act on the cluster as it was built
	// (deploy, down, block teardown): a flag contradicting the record is an
	// error.
	matchShape shapeUse = iota
	// reshape is for commands that build the cluster to their flags (up,
	// cluster create, block setup): a flag the user passed replaces the record.
	reshape
)

// checkShapeFlags refuses a size flag the user set to a value no cluster can
// have: --workers below minWorkers, a negative --disk-count, or a --disk-size
// below 1 GiB. A command runs it before it names or locks its cluster, so a
// refused run leaves nothing behind. An unset flag holds a valid default or
// takes the cluster's recorded value, and is not checked; nor is a deprecated
// one, which nothing reads.
func checkShapeFlags(cmd *cobra.Command, minWorkers int) error {
	for _, f := range []struct {
		name  string
		floor int
	}{{"workers", minWorkers}, {"disk-count", 0}, {"disk-size", 1}} {
		if fl := cmd.Flags().Lookup(f.name); fl == nil || !fl.Changed || fl.Deprecated != "" {
			continue
		}
		v, err := cmd.Flags().GetInt(f.name)
		if err != nil {
			return err
		}
		switch {
		case v >= f.floor:
		case f.floor == 1:
			return fmt.Errorf("--%s must be more than 0, not %d", f.name, v)
		default:
			return fmt.Errorf("--%s must be %d or more, not %d", f.name, f.floor, v)
		}
	}
	return nil
}

// useRecordedShape settles a command's shape flags against the shape recorded
// for its cluster: every flag the user did not pass takes the recorded value.
// changed reports whether the user passed the command's flag of that name.
func useRecordedShape(name string, changed func(flag string) bool, use shapeUse,
	workers, diskCount *int, iqnDate *string) error {

	rec, ok := readShape(name)
	w, err := resolveRecorded(use, "workers", *workers, changed("workers"), rec.Workers, ok)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", name, err)
	}
	d, err := resolveRecorded(use, "disk-count", *diskCount, changed("disk-count"), rec.DiskCount, ok)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", name, err)
	}
	q, err := resolveRecorded(use, "iqn-date", *iqnDate, changed("iqn-date"), rec.IQNDate, ok)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", name, err)
	}
	*workers, *diskCount, *iqnDate = w, d, q
	return nil
}

// resolveRecorded picks the value a command uses for one shape flag. val is the
// flag's current value — what the user passed when explicit is true, otherwise
// its default — and recorded is the cluster's recorded value, meaningful only
// when haveRecord is true.
func resolveRecorded[T comparable](use shapeUse, flag string, val T, explicit bool, recorded T, haveRecord bool) (T, error) {
	switch {
	case !haveRecord:
		return val, nil
	case !explicit:
		return recorded, nil
	case val != recorded && use == matchShape:
		return val, fmt.Errorf("--%s %v contradicts the recorded --%s %v; omit --%s to use the recorded value",
			flag, val, flag, recorded, flag)
	}
	return val, nil
}
