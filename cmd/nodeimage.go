package cmd

import (
	"io"

	"github.com/jhoblitt/rooket/internal/run"
)

// defaultNodeImage is the kindest/node image rooket pins by default. It is the
// exact ref — tag AND digest — that the bundled kind (v0.29.0) uses as its own
// built-in default, so pinning it changes no Kubernetes version: it only makes
// the choice explicit, reproducible, and pre-pullable. The digest is what lets
// prePullNodeImage warm the exact image kind will boot, so kind finds it
// present and never re-pulls. Bump both together with the kind version rooket
// targets (kind's release notes name the current default node image).
const defaultNodeImage = "kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f"

// prePullNodeImage pulls the kind node image ahead of `kind create cluster` so
// the (large) image download overlaps other bring-up work instead of sitting
// on cluster create's critical path. It is best-effort: any failure (offline,
// a bad ref) is warned and swallowed — never returned — so kind still falls
// back to pulling the image itself. With the digest-pinned default ref the
// engine satisfies an already-present image from the local store, so a warm
// host pays only a fast metadata check rather than a re-download.
func prePullNodeImage(out io.Writer, image string) {
	if image == "" {
		return
	}
	run.Fprintf(out, "==> pre-pulling kind node image %s\n", image)
	if err := run.CmdTo(out, containerEngine.String(), "pull", image); err != nil {
		run.Fprintf(out, "warning: node-image pre-pull failed (%v); kind will pull it during cluster create\n", err)
	}
}
