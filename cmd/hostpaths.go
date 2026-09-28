package cmd

import "github.com/jhoblitt/rooket/internal/lio"

// These are funcs rather than constants so that tests can point them at trees
// of their own: no test may depend on the iSCSI targets or sessions of the
// machine it runs on.
var (
	// hostLIORoot is where rooket reads the kernel's LIO configuration.
	hostLIORoot = func() string { return lio.DefaultRoot }
	// hostByPathDir is where rooket reads the host's /dev/disk/by-path links.
	hostByPathDir = func() string { return iscsiByPathDir }
)
