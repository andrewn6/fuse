package main

import (
	"context"
	"fmt"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// Resolving the setup layer cache, client-side.
//
// The client owns resolution because it is the only thing that can derive a
// layer key at all: a key folds in the digest of the step's declared `inputs`,
// and only the client can read the files those patterns match. So the flow is
// resolve here, then hand the winner to the orchestrator as a seed snapshot id,
// rather than shipping the recipe to the control plane and asking it.

// targetArch reports the architecture layers for this build will be filed
// under, or "" when it cannot be determined.
//
// This is the one place the arch decision (see fusefile.LayerKeys) becomes
// concrete. Arch is deliberately not part of a layer key, because a key is
// derived before any host is scheduled and the client's own architecture says
// nothing about the fleet's. It is instead stamped onto the artifact by the
// host that built it, so a lookup has to supply the arch of the host this build
// will land on.
//
// A wrong answer here is worse than no answer: filing a layer under an arch it
// was not built on means a later lookup hits that key and is served a rootfs it
// cannot boot. So this returns "" rather than guessing, and every caller
// degrades to a cold build on "". It never falls back to runtime.GOARCH, which
// is exactly the bug this design removed.
func targetArch(ctx context.Context, cl *fuse.Client, activeHost string) string {
	// a scoped host is an explicit answer: the operator already said where this
	// work goes, so there is nothing to infer.
	if activeHost != "" {
		h, err := cl.Hosts.Get(ctx, activeHost)
		if err != nil || h == nil {
			return ""
		}
		return hostTargetArch(*h)
	}

	hosts, err := cl.Hosts.List(ctx)
	if err != nil || len(hosts) == 0 {
		return ""
	}
	// a single-architecture fleet has one answer and it does not matter which
	// host wins the placement. a mixed fleet has no answer that is safe to
	// assume, so it degrades to a cold build rather than filing a layer under a
	// coin flip.
	arch := ""
	for _, h := range hosts {
		a := hostTargetArch(h)
		if arch == "" {
			arch = a
			continue
		}
		if a != arch {
			return ""
		}
	}
	return arch
}

// hostTargetArch reports the arch a layer built on this host is filed under.
//
// An empty arch on the host record means amd64, which is what the scheduler and
// the snapshot write path both already assume (0010_host_arch.sql: every host
// registered before that column existed was x86_64). Mirroring that here is not
// a guess, it is what makes a lookup match what the write path stamped; getting
// it wrong would simply mean never hitting on an unmigrated host.
func hostTargetArch(h fuse.Host) string {
	if h.Capacity.Arch == "" {
		return "amd64"
	}
	return h.Capacity.Arch
}

// layerHit is a resolved cache hit: the index of the deepest setup step whose
// layer already exists, and the artifact to seed from.
type layerHit struct {
	Index    int
	Snapshot *fuse.Snapshot
}

// resolveDeepestLayer walks the layer chain backwards and returns the deepest
// step already in the store.
//
// Deepest-first with an early exit is the whole point: the chain is ordered so
// that a hit at step N means every step before it is also satisfied by that one
// artifact, so the first hit found walking backwards is the most work that can
// be skipped, and no shallower key needs to be asked about at all. A warm cache
// therefore costs exactly one round trip.
//
// A miss is not an error, and neither is a failed probe. The cache is an
// optimization; an orchestrator that cannot answer right now should cost the
// operator time, never their build. Probe failures are surfaced to the caller
// so it can say why the build went cold instead of silently taking the slow
// path.
func resolveDeepestLayer(ctx context.Context, cl *fuse.Client, p *layerPlan, arch string) (*layerHit, error) {
	if p == nil || arch == "" {
		return nil, nil
	}
	for i := len(p.Steps) - 1; i >= 0; i-- {
		k := p.Steps[i]
		if !k.Cacheable {
			continue
		}
		snap, ok, err := cl.Snapshots.Resolve(ctx, k.Key, arch)
		if err != nil {
			return nil, fmt.Errorf("resolve layer for setup[%d]: %w", i, err)
		}
		if ok && snap != nil {
			return &layerHit{Index: i, Snapshot: snap}, nil
		}
	}
	return nil, nil
}

// cacheablePrefixLen returns how many leading setup steps can produce a layer.
//
// Cacheable steps are always a prefix, because the chain breaks permanently at
// the first step that opts out (fusefile.LayerKeys sets chainBroken and never
// clears it). That is what bounds the stepped build loop: everything from here
// to the end runs as one exec, since none of it can be snapshotted anyway.
func cacheablePrefixLen(p *layerPlan) int {
	if p == nil {
		return 0
	}
	n := 0
	for _, k := range p.Steps {
		if !k.Cacheable {
			break
		}
		n++
	}
	return n
}
