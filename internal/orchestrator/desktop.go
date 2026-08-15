package orchestrator

// The environment-level desktop: the geometry an author declares in the
// Fusefile's `desktop:` block, delivered to the guest as a file the same way
// the healthcheck config is (see health.go for why a file and not a flag: the
// firecracker host agent ignores AgentSpec.Command and sends structured paths
// on a frozen wire).
//
// The orchestrator does not act on the geometry itself. The desktop image's
// display runner reads this file when the display starts, and fused compares
// it against the live display and restarts the display units on a mismatch.
// An environment with no desktop block writes no file, and the image's baked
// default applies.

// GuestDesktopPath is where the desktop geometry lands in the guest,
// alongside the manifest and secrets under /fuse.
const GuestDesktopPath = "/fuse/desktop.json"

// DesktopSpec is the desktop geometry as it travels from the create request
// to the guest.
//
// The json tags are the guest wire: this struct is marshalled straight into
// GuestDesktopPath, so the display runner's config shape and this one are the
// same shape by construction. Both fields are required and validated at the
// API boundary; there is no "unset means default" here, because a guessed
// dimension would silently shift every coordinate a computer-use model emits.
type DesktopSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
