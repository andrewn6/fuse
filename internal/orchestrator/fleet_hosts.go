package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// ErrHostNotFound is returned when a host operation targets an
// unregistered host ID.
var ErrHostNotFound = fmt.Errorf("host not found")

// RegisterHost adds a host to the scheduler's registry. If the host
// already exists, its capacity, URL, token, and region are updated
// (useful for heartbeat-like refreshes). The host starts in
// HostActive state.
//
// The caller must supply a Provider for this host (typically a
// firecracker.Provider constructed with the host's URL/token).
// FleetManager holds only the Provider interface to avoid import
// cycles with concrete provider packages.
func (fm *FleetManager) RegisterHost(ctx context.Context, h Host, p Provider) error {
	now := time.Now()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	h.UpdatedAt = now
	h.LastSeen = now
	if h.State == "" {
		h.State = HostActive
	}
	if h.Backend == "" {
		h.Backend = BackendFirecracker
	}

	fm.mu.Lock()
	fm.hosts[h.ID] = &h
	if existing, ok := fm.hostProviders[h.ID]; ok {
		_ = existing.Close()
	}
	fm.hostProviders[h.ID] = p
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.UpsertHost(ctx, fm.hostToRecord(h)); err != nil {
			return fmt.Errorf("persist host %s: %w", h.ID, err)
		}
	}
	fm.appendEvent(ctx, "host", h.ID, "host.registered", map[string]any{
		"url":    h.URL,
		"region": h.Region,
		"cpus":   h.Capacity.CPUs,
		"ram_mb": h.Capacity.RamMB,
	})
	return nil
}

// CordonHost marks a host as cordoned (no new VMs). Existing VMs
// are left running.
func (fm *FleetManager) CordonHost(ctx context.Context, hostID string) error {
	return fm.setHostState(ctx, hostID, HostCordoned, "host.cordoned")
}

// UncordonHost returns a cordoned or draining host to active
// scheduling.
func (fm *FleetManager) UncordonHost(ctx context.Context, hostID string) error {
	return fm.setHostState(ctx, hostID, HostActive, "host.uncordoned")
}

func (fm *FleetManager) setHostState(ctx context.Context, hostID string, state HostState, event string) error {
	fm.mu.Lock()
	h, ok := fm.hosts[hostID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrHostNotFound, hostID)
	}
	h.State = state
	h.UpdatedAt = time.Now()
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.UpsertHost(ctx, fm.hostToRecord(*h)); err != nil {
			return fmt.Errorf("persist host state %s: %w", hostID, err)
		}
	}
	fm.appendEvent(ctx, "host", hostID, event, map[string]any{"state": string(state)})
	return nil
}

// RemoveHost deletes a host from the registry. It must have no VMs
// assigned to it; callers should cordon/drain and wait for VMs to
// leave before removing.
func (fm *FleetManager) RemoveHost(ctx context.Context, hostID string) error {
	fm.mu.Lock()
	_, ok := fm.hosts[hostID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrHostNotFound, hostID)
	}

	// Check that no tracked VMs reference this host.
	for _, v := range fm.vms {
		if v.hostID == hostID {
			fm.mu.Unlock()
			return fmt.Errorf("%w: host %s still has vm %s assigned", ErrHostHasVMs, hostID, v.id)
		}
	}

	if p, exists := fm.hostProviders[hostID]; exists {
		_ = p.Close()
		delete(fm.hostProviders, hostID)
	}
	delete(fm.hosts, hostID)
	fm.mu.Unlock()

	if fm.store != nil {
		if err := fm.store.DeleteHost(ctx, hostID); err != nil {
			return fmt.Errorf("delete host %s: %w", hostID, err)
		}
	}
	fm.appendEvent(ctx, "host", hostID, "host.removed", nil)
	return nil
}

// GetHost returns a snapshot of a registered host.
func (fm *FleetManager) GetHost(hostID string) (Host, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	if !ok {
		return Host{}, false
	}
	return *h, true
}

// HostFilter controls pagination for ListHostsFiltered. There is no
// exact-match filtering today (registration/listing never narrowed hosts by
// field) — this only carries Limit/Cursor, kept as its own type for
// symmetry with VMFilter/SnapshotFilter and so a future field doesn't need
// a signature change.
type HostFilter struct {
	Limit  int
	Cursor string
}

// hostCursor is the opaque cursor payload for host listing. Host ids are
// operator-chosen and unique, so an ascending id sort is a stable
// pagination key.
type hostCursor struct {
	ID string `json:"id"`
}

// ListHosts returns every registered host, transparently paginating
// through the full result set. Intended for internal/test callers that
// want "everything"; the REST API uses ListHostsFiltered directly so it
// can return a cursor to the caller.
func (fm *FleetManager) ListHosts() []Host {
	var out []Host
	cursor := ""
	for {
		page, next, err := fm.ListHostsFiltered(HostFilter{Limit: MaxPageLimit, Cursor: cursor})
		if err != nil {
			return out
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return out
}

// ListHostsFiltered returns one page of registered hosts sorted ascending
// by host id. It returns the page, a next-page cursor (empty when there
// are no more results), and an error only for a malformed cursor.
func (fm *FleetManager) ListHostsFiltered(filter HostFilter) ([]Host, string, error) {
	limit := clampLimit(filter.Limit)

	var after string
	if filter.Cursor != "" {
		var c hostCursor
		if err := decodeCursor(filter.Cursor, &c); err != nil {
			return nil, "", err
		}
		after = c.ID
	}

	fm.mu.RLock()
	all := make([]Host, 0, len(fm.hosts))
	for _, h := range fm.hosts {
		all = append(all, *h)
	}
	fm.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	start := 0
	if after != "" {
		start = sort.Search(len(all), func(i int) bool { return all[i].ID > after })
	}

	end := len(all)
	var next string
	if start+limit < end {
		end = start + limit
		next, _ = encodeCursor(hostCursor{ID: all[end-1].ID})
	}
	if start > end {
		start = end
	}
	return all[start:end], next, nil
}

// activeHosts returns a point-in-time snapshot of all hosts eligible for
// scheduling. Called under NO lock — takes its own read lock. It returns
// independent *copies* (not the live map pointers) so the pure Schedule()
// function can read host capacity/allocation without a lock while another
// goroutine mutates the real hosts under fm.mu in allocateOnHost. Returning
// the live pointers here is a data race (Schedule read vs allocateOnHost
// write); the copies make each scheduling decision operate on a consistent
// snapshot.
func (fm *FleetManager) activeHosts() []*Host {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.activeHostsLocked()
}

// activeHostsLocked returns host copies while the caller holds fm.mu.
func (fm *FleetManager) activeHostsLocked() []*Host {
	out := make([]*Host, 0, len(fm.hosts))
	for _, h := range fm.hosts {
		hc := *h // copy the Host (Capacity/Allocated are value structs)
		// deep-copy the GPU slices: the value copy above shares their backing
		// arrays with the live host, and allocate/deallocate mutate them.
		hc.Capacity.GPUDevices = append([]GPUDevice(nil), h.Capacity.GPUDevices...)
		hc.Allocated.GPUDeviceUUIDs = append([]string(nil), h.Allocated.GPUDeviceUUIDs...)
		// deep-copy the per-instance MIG slices for the same reason.
		hc.Capacity.MIGInstances = append([]MIGInstance(nil), h.Capacity.MIGInstances...)
		hc.Allocated.MIGInstanceUUIDs = append([]string(nil), h.Allocated.MIGInstanceUUIDs...)
		out = append(out, &hc)
	}
	return out
}

// allocateOnHost increments the allocated counters for a host after
// a successful placement and persists the new totals asynchronously
// so they survive an orchestrator restart. Must be called under
// fm.mu.Lock.
//
// When the host reports per-device GPU inventory (Capacity.GPUDevices
// non-empty), it binds spec.GPUs concrete free device uuids to the VM:
// the uuids are recorded on v.spec.GPUUUIDs (durable per-VM binding) and
// added to the host's Allocated.GPUDeviceUUIDs set, and Allocated.GPUs is
// kept equal to the number of bound uuids. On legacy homogeneous hosts
// (no per-device inventory) it falls back to the scalar counter.
func (fm *FleetManager) allocateOnHost(hostID string, v *vm) {
	h, ok := fm.hosts[hostID]
	if !ok {
		return
	}
	spec := v.spec
	h.Allocated.CPUs += spec.CPUs
	h.Allocated.RamMB += spec.RamMB
	h.Allocated.StorageGB += spec.StorageGB
	h.Allocated.VMCount++
	switch {
	case spec.GPUProfile != "":
		// Fractional allocation: consume MIG instances, not whole devices.
		if len(h.Capacity.MIGInstances) > 0 {
			// per-instance host: bind concrete free instance uuids to the VM
			// and record them on the host's allocated set, keeping the
			// derived MIGProfiles count map in sync.
			free := freeMIGInstances(h.Capacity, h.Allocated, spec.GPUProfile, spec.GPUKind)
			n := int(spec.GPUs)
			if n > len(free) {
				// scheduler admitted this placement, so free should be
				// sufficient; clamp defensively rather than index out of range.
				n = len(free)
			}
			// size the backing array from the host inventory (len(free), a
			// small non-user-controlled count), not from spec.GPUs, so a
			// hostile gpu count can't drive an oversized allocation.
			bound := make([]string, 0, len(free))
			for _, inst := range free[:n] {
				bound = append(bound, inst.UUID)
			}
			v.spec.MIGInstanceUUIDs = append([]string(nil), bound...)
			h.Allocated.MIGInstanceUUIDs = append(h.Allocated.MIGInstanceUUIDs, bound...)
			h.Allocated.MIGProfiles = migProfileCounts(h.Allocated.MIGInstanceUUIDs, h.Capacity.MIGInstances)
		} else {
			// legacy count-map host.
			if h.Allocated.MIGProfiles == nil {
				h.Allocated.MIGProfiles = make(map[string]int)
			}
			h.Allocated.MIGProfiles[spec.GPUProfile] += int(spec.GPUs)
		}
	case spec.GPUs > 0 && len(h.Capacity.GPUDevices) > 0:
		free := freeMatchingDevices(h.Capacity, h.Allocated, spec.GPUKind)
		n := int(spec.GPUs)
		if n > len(free) {
			// scheduler admitted this placement, so free should be
			// sufficient; clamp defensively rather than index out of range.
			n = len(free)
		}
		bound := free[:n]
		v.spec.GPUUUIDs = append([]string(nil), bound...)
		h.Allocated.GPUDeviceUUIDs = append(h.Allocated.GPUDeviceUUIDs, bound...)
		h.Allocated.GPUs = len(h.Allocated.GPUDeviceUUIDs)
	default:
		h.Allocated.GPUs += int(spec.GPUs)
	}
	h.UpdatedAt = time.Now()
	fm.persistHostRecordBackground(fm.hostToRecord(*h))
}

// deallocateOnHost decrements the allocated counters when a VM is
// destroyed and persists the new totals asynchronously. Must be
// called under fm.mu.Lock.
func (fm *FleetManager) deallocateOnHost(hostID string, spec Spec) {
	h, ok := fm.hosts[hostID]
	if !ok {
		return
	}
	h.Allocated.CPUs -= spec.CPUs
	if h.Allocated.CPUs < 0 {
		h.Allocated.CPUs = 0
	}
	h.Allocated.RamMB -= spec.RamMB
	if h.Allocated.RamMB < 0 {
		h.Allocated.RamMB = 0
	}
	h.Allocated.StorageGB -= spec.StorageGB
	if h.Allocated.StorageGB < 0 {
		h.Allocated.StorageGB = 0
	}
	switch {
	case spec.GPUProfile != "":
		if len(spec.MIGInstanceUUIDs) > 0 {
			// per-instance host: release exactly the MIG instance uuids this
			// VM held, then re-derive the count map from the survivors so a
			// stale counter can never drift from the bound-uuid set.
			release := make(map[string]struct{}, len(spec.MIGInstanceUUIDs))
			for _, u := range spec.MIGInstanceUUIDs {
				release[u] = struct{}{}
			}
			var kept []string
			for _, u := range h.Allocated.MIGInstanceUUIDs {
				if _, drop := release[u]; drop {
					continue
				}
				kept = append(kept, u)
			}
			h.Allocated.MIGInstanceUUIDs = kept
			h.Allocated.MIGProfiles = migProfileCounts(h.Allocated.MIGInstanceUUIDs, h.Capacity.MIGInstances)
		} else if n := h.Allocated.MIGProfiles[spec.GPUProfile] - int(spec.GPUs); n > 0 {
			// legacy count-map host.
			h.Allocated.MIGProfiles[spec.GPUProfile] = n
		} else if h.Allocated.MIGProfiles != nil {
			delete(h.Allocated.MIGProfiles, spec.GPUProfile)
		}
	case len(h.Capacity.GPUDevices) > 0 && len(spec.GPUUUIDs) > 0:
		// per-device host: release exactly the uuids this vm held and keep
		// the scalar counter equal to the remaining bound-uuid count.
		release := make(map[string]struct{}, len(spec.GPUUUIDs))
		for _, u := range spec.GPUUUIDs {
			release[u] = struct{}{}
		}
		// build a fresh slice rather than truncating in place: host copies
		// handed to the scheduler share this backing array, so reusing it
		// could corrupt a concurrently-held snapshot.
		var kept []string
		for _, u := range h.Allocated.GPUDeviceUUIDs {
			if _, drop := release[u]; drop {
				continue
			}
			kept = append(kept, u)
		}
		h.Allocated.GPUDeviceUUIDs = kept
		h.Allocated.GPUs = len(h.Allocated.GPUDeviceUUIDs)
	default:
		h.Allocated.GPUs -= int(spec.GPUs)
		if h.Allocated.GPUs < 0 {
			h.Allocated.GPUs = 0
		}
	}
	h.Allocated.VMCount--
	if h.Allocated.VMCount < 0 {
		h.Allocated.VMCount = 0
	}
	h.UpdatedAt = time.Now()
	fm.persistHostRecordBackground(fm.hostToRecord(*h))
}

// providerForHost returns the cached provider for the given host ID.
// Caller must hold fm.mu (read or write).
func (fm *FleetManager) providerForHost(hostID string) (Provider, bool) {
	p, ok := fm.hostProviders[hostID]
	return p, ok
}

// providerForVM returns the provider that owns a vm. Hosted vms must never
// fall back to the default provider because that can leak the real vm.
func (fm *FleetManager) providerForVM(hostID string) (Provider, error) {
	if hostID == "" {
		return fm.provider, nil
	}
	fm.mu.RLock()
	p, ok := fm.providerForHost(hostID)
	fm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider for host %s not found", hostID)
	}
	return p, nil
}

// listAllHostVMs collects VMs from all registered host providers.
// Used by reconcile when the fleet is in multi-host mode.
func (fm *FleetManager) listAllHostVMs(ctx context.Context) ([]Environment, map[string]Provider, error) {
	fm.mu.RLock()
	providers := make(map[string]Provider, len(fm.hostProviders))
	for id, p := range fm.hostProviders {
		providers[id] = p
	}
	fm.mu.RUnlock()

	var (
		mu     sync.Mutex
		all    []Environment
		owners = make(map[string]Provider)
		errs   []error
	)

	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			envs, err := p.List(ctx, fm.prefix)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				all = append(all, envs...)
				for _, env := range envs {
					owners[env.Name()] = p
				}
			}
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if len(errs) > 0 {
		return all, owners, fmt.Errorf("list vms from %d hosts: %d errors (first: %w)", len(providers), len(errs), errs[0])
	}
	return all, owners, nil
}

// ── Conversion helpers ────────────────────────────────────────────

// hostToRecord builds a durable HostRecord from an in-memory Host. The
// agent bearer token is encrypted with fm.tokenEncryptionKey so it
// never reaches the database in cleartext. If the key is not
// configured the token is stored verbatim — caller is expected to
// reject this configuration when running against shared infra
// (TestFleetManager / dev only).
func (fm *FleetManager) hostToRecord(h Host) HostRecord {
	r := HostRecord{
		ID:        h.ID,
		URL:       h.URL,
		Region:    h.Region,
		State:     h.State,
		Backend:   h.Backend,
		Labels:    h.Labels,
		Capacity:  h.Capacity,
		Allocated: h.Allocated,
		LastSeen:  h.LastSeen,
		CreatedAt: h.CreatedAt,
		UpdatedAt: h.UpdatedAt,
	}
	if h.Token == "" {
		return r
	}
	if len(fm.tokenEncryptionKey) == 32 {
		ciphertext, err := secrets.EncryptToken(h.Token, fm.tokenEncryptionKey)
		if err == nil {
			r.TokenEncrypted = ciphertext
			return r
		}
		fm.logger.Warn("encrypt host token failed; storing plaintext", "host", h.ID, "err", err)
	}
	// Fallback: store the token bytes directly. Legacy behavior, only
	// reached when TOKEN_ENCRYPTION_KEY is unset (dev / in-memory tests).
	r.TokenEncrypted = []byte(h.Token)
	return r
}

// hostFromRecord rebuilds an in-memory Host from a HostRecord, decrypting
// the bearer token. If decryption fails (wrong key, unencrypted legacy
// row), falls back to treating the bytes as a plaintext token so
// recovered state still produces a working provider client.
func (fm *FleetManager) hostFromRecord(r HostRecord) Host {
	h := Host{
		ID:        r.ID,
		URL:       r.URL,
		Region:    r.Region,
		State:     r.State,
		Backend:   r.Backend,
		Labels:    r.Labels,
		Capacity:  r.Capacity,
		Allocated: r.Allocated,
		LastSeen:  r.LastSeen,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if len(r.TokenEncrypted) == 0 {
		return h
	}
	if len(fm.tokenEncryptionKey) == 32 {
		plain, err := secrets.DecryptToken(r.TokenEncrypted, fm.tokenEncryptionKey)
		if err == nil {
			h.Token = plain
			return h
		}
		// A configured 32-byte key that cannot decrypt this row means the
		// key changed (or the row is corrupt), not a legacy plaintext
		// token. Using the raw ciphertext bytes as a bearer token would
		// silently 401 every call to this host with garbage that looks
		// like a real token. Fail loud instead: leave the token empty so
		// the failure is a clean, diagnosable "rejected the token", and
		// tell the operator the key is the problem.
		fm.logger.Error("host token decrypt failed; leaving token empty (check TOKEN_ENCRYPTION_KEY has not changed)",
			"host_id", r.ID, "error", err)
		return h
	}
	// No encryption key configured: the row holds a plaintext token stored
	// on a keyless (legacy / dev) orchestrator.
	h.Token = string(r.TokenEncrypted)
	return h
}
