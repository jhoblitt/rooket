package cmd

import (
	"strings"
	"testing"
)

// defaultNodeImage must pin an exact ref (tag AND digest): the digest is what
// lets the pre-pull warm the very image kind boots, so kind finds it present
// and never re-pulls. A tag-only ref could drift from what kind resolves.
func TestDefaultNodeImageIsPinnedByDigest(t *testing.T) {
	if !strings.HasPrefix(defaultNodeImage, "kindest/node:") {
		t.Errorf("defaultNodeImage %q must be a kindest/node image", defaultNodeImage)
	}
	if !strings.Contains(defaultNodeImage, "@sha256:") {
		t.Errorf("defaultNodeImage %q must pin a digest so the pre-pull matches kind's own ref", defaultNodeImage)
	}
}

// The up and create commands must default --node-image to the pinned image, so
// the out-of-the-box run is reproducible and pre-pullable.
func TestNodeImageFlagDefaults(t *testing.T) {
	if got := upCmd.Flags().Lookup("node-image"); got == nil {
		t.Fatal("up has no --node-image flag")
	} else if got.DefValue != defaultNodeImage {
		t.Errorf("up --node-image default = %q, want %q", got.DefValue, defaultNodeImage)
	}
	if got := createCmd.Flags().Lookup("node-image"); got == nil {
		t.Fatal("create has no --node-image flag")
	} else if got.DefValue != defaultNodeImage {
		t.Errorf("create --node-image default = %q, want %q", got.DefValue, defaultNodeImage)
	}
}
