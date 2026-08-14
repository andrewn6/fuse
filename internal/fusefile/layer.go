package fusefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// layerKeyDomain namespaces the layer key hash. bump the version suffix if the
// preimage layout below ever changes, so old keys can never collide with new
// ones and every existing layer is invalidated at once.
const layerKeyDomain = "fusefile-layer/v2"

// reasons a step is not cacheable. these are stable strings so callers (the
// `--plan` renderer, and per-step hit/miss reporting) can branch on them
// instead of matching prose.
const (
	// ReasonCacheDisabled means the Fusefile has no `cache: {enabled: true}`.
	ReasonCacheDisabled = "caching is not enabled"
	// ReasonStepOptedOut means the step set `cache: false`.
	ReasonStepOptedOut = "step opted out with cache: false"
	// ReasonParentUncacheable means an earlier step is uncacheable, so this
	// step has no known parent layer to build on.
	ReasonParentUncacheable = "an earlier step is uncacheable"
	// ReasonGPU means the environment requests a GPU. GPU environments run on
	// the qemu backend, whose snapshot endpoints hard-501 because a vfio
	// device cannot be checkpointed, so there is nowhere to store a layer.
	ReasonGPU = "gpu environments cannot be snapshotted"
)

// InputDigester hashes a step's declared `inputs` patterns and returns the
// digest to fold into that step's key. It is supplied by the caller because
// this package deliberately does no filesystem IO: only the CLI knows the
// directory the Fusefile was read from. It must return "" for an empty
// pattern list.
type InputDigester func(patterns []string) (string, error)

// LayerOptions carries the key component that is not derivable from the
// Fusefile itself.
type LayerOptions struct {
	// BaseKey identifies the base rootfs the first step builds on. Empty
	// means derive it from Fusefile.Image via BaseKey.
	//
	// Ideally this is a digest of the resolved rootfs bytes; the host agent
	// does not expose one yet (it resolves `image` to a file by name with no
	// content hash), so today it is a digest of the image reference. That is
	// a weaker binding: republishing the same tag with new contents does not
	// invalidate. Wiring a real rootfs digest is backend work.
	BaseKey string
}

// LayerKey is the derived cache identity of one build step.
type LayerKey struct {
	// Index is the step's position in the build phase (Fusefile.BuildSteps).
	Index int
	// Script is the shell fragment this step emits, hashed byte for byte.
	Script string
	// Key is the step's content-addressed layer key, hex sha256. Empty when
	// the step is not cacheable.
	Key string
	// ParentKey is the key this step was chained onto: the previous step's
	// key, or the base key for the first step. Empty when not cacheable.
	ParentKey string
	// InputsDigest is the digest of the step's declared inputs, "" when the
	// step declares none.
	InputsDigest string
	// Cacheable reports whether this step can produce or restore a layer.
	Cacheable bool
	// Reason explains why Cacheable is false; empty when it is true.
	Reason string
}

// LayerKeys derives the layer key of every build step, in order.
//
// The key is a chained hash, so it invalidates in exactly one direction: edit
// step N and steps N..end all get new keys, while steps before N are
// untouched. That is also why a miss cascades: step N+1's key is defined in
// terms of step N's, so a step whose parent is unknown cannot be keyed at all
// and neither can anything after it.
//
//	layer_key(i) = sha256(
//	    "fusefile-layer/v2\n" +
//	    parent_key(i) + "\n" +   // layer_key(i-1), or the base key for i == 0
//	    step_script(i) + "\n" +  // the fragment the step emits, byte for byte
//	    inputs_digest(i) + "\n" +
//	    workspace)               // setup runs after `cd <workspace>`
//
// Deliberately excluded: secret values (a layer must not be keyed on material
// it is not allowed to bake in, so a step that reads secrets opts out
// instead), the task id, every `resources` field (none of them change the
// filesystem), and `run:` (it is the entrypoint, not a layer).
//
// Arch is excluded too, but for a different reason: it is a real constraint,
// just not a derivable one. An ext4 rootfs genuinely is not portable across
// architectures, so an arm64 artifact must never be served to an amd64 boot.
// The problem is that nothing here knows the answer. The key is derived
// client-side, before any host has been scheduled, so the only arch available
// at this point is the operator's laptop, which has no relationship to the
// host the build lands on. Folding that in would produce a key that asserts
// something false: an operator on an arm64 Mac building on an amd64 host would
// label the artifact arm64, and a later genuinely-arm64 lookup would hit that
// key and be handed amd64 bytes. A key that lies is worse than a key that
// misses. So arch is recorded on the artifact from the host that actually
// built it, and applied as a separate equality filter at lookup time
// alongside the tenant and the key.
//
// LayerKeys is pure apart from the digester it is handed, so it can be tested
// without touching a disk.
func LayerKeys(f *Fusefile, inputs InputDigester, opts LayerOptions) ([]LayerKey, error) {
	steps := f.BuildSteps()
	if len(steps) == 0 {
		return nil, nil
	}

	baseKey := opts.BaseKey
	if baseKey == "" {
		baseKey = BaseKey(f.Image, f.Files)
	}
	workspace := workspaceOf(f)

	scripts := setupScripts(f)
	out := make([]LayerKey, len(steps))

	// global, when set, applies to every step: nothing about this environment
	// can be cached at all.
	global := ""
	switch {
	case !f.Cache.Enabled:
		global = ReasonCacheDisabled
	case f.Resources.GPU > 0:
		global = ReasonGPU
	}

	// chainBroken records that an earlier step opted out, so from here on
	// there is no parent key to hash against and no key exists.
	chainBroken := false
	parent := baseKey

	for i, step := range steps {
		lk := LayerKey{Index: i, Script: scripts[i]}

		switch {
		case global != "":
			lk.Reason = global
		case chainBroken:
			lk.Reason = ReasonParentUncacheable
		case !step.cacheable():
			lk.Reason = ReasonStepOptedOut
			chainBroken = true
		default:
			digest, err := digestInputs(inputs, step.Inputs)
			if err != nil {
				return nil, fmt.Errorf("%s[%d].inputs: %w", f.buildField(), i, err)
			}
			lk.InputsDigest = digest
			lk.ParentKey = parent
			lk.Key = hashLayer(parent, scripts[i], digest, workspace)
			lk.Cacheable = true
			parent = lk.Key
		}

		out[i] = lk
	}

	return out, nil
}

// digestInputs returns "" for a step with no declared inputs and otherwise
// delegates to the caller's digester.
func digestInputs(inputs InputDigester, patterns []string) (string, error) {
	if len(patterns) == 0 {
		return "", nil
	}
	if inputs == nil {
		return "", fmt.Errorf("no input digester was supplied")
	}
	return inputs(patterns)
}

// hashLayer computes one link of the chain. every component is newline
// separated so no two different component splits can produce the same
// preimage.
func hashLayer(parent, script, inputsDigest, workspace string) string {
	h := sha256.New()
	for _, part := range []string{layerKeyDomain, parent, script, inputsDigest} {
		h.Write([]byte(part))
		h.Write([]byte("\n"))
	}
	h.Write([]byte(workspace))
	return hex.EncodeToString(h.Sum(nil))
}

// BaseKey derives the root of the chain from the base image reference and the
// file block. An empty image means the host's baked base rootfs, which still
// gets a distinct key rather than an empty one so the first step's preimage is
// never ambiguous.
//
// Files belong here rather than in any step's key because they are written
// before the first setup step runs, so they are part of the state that step
// builds on. Leaving them out would let an edited `files:` entry serve a layer
// baked against the old contents.
func BaseKey(image string, files []File) string {
	h := sha256.New()
	h.Write([]byte(image))
	// length-prefixed so ("ab", "c") and ("a", "bc") cannot collide.
	// a hash.Hash never returns an error, but errcheck does not know that.
	_, _ = fmt.Fprintf(h, "\x00files:%d\x00", len(files))
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%d:%s\x00%d:%s\x00",
			len(f.Path), f.Path, len(f.Content), f.Content)
	}
	return "image:" + hex.EncodeToString(h.Sum(nil))
}

// cacheable reports whether the step opted out of caching. an unset `cache`
// field means "cacheable", so only an explicit `cache: false` opts out.
func (s Step) cacheable() bool {
	return s.Cache == nil || *s.Cache
}
