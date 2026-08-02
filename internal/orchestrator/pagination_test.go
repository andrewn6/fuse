package orchestrator

import (
	"context"
	"fmt"
	"testing"
)

// TestListFleetFiltered_paginatesAndRoundTripsCursor provisions more VMs
// than one default page and walks every page via the returned cursor,
// checking the pages are disjoint, in ascending id order, and reassemble
// to the full set.
func TestListFleetFiltered_paginatesAndRoundTripsCursor(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()

	const total = 120
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		taskID := fmt.Sprintf("task-%03d", i)
		if _, err := fm.ProvisionAndAssign(ctx, taskID, Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
		want["fuse-"+taskID] = true
	}

	const pageSize = 25
	seen := make(map[string]bool, total)
	var lastID string
	cursor := ""
	pages := 0
	for {
		page, next, err := fm.ListFleetFiltered(VMFilter{Limit: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > total { // guard against an infinite loop on a bug
			t.Fatalf("did not terminate after %d pages", pages)
		}
		if len(page) == 0 {
			t.Fatalf("page %d was empty with a non-terminal call", pages)
		}
		for _, v := range page {
			if seen[v.ID] {
				t.Fatalf("id %s returned twice across pages", v.ID)
			}
			seen[v.ID] = true
			if lastID != "" && v.ID <= lastID {
				t.Fatalf("ids not ascending across pages: %s then %s", lastID, v.ID)
			}
			lastID = v.ID
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != total {
		t.Fatalf("saw %d unique vms across %d pages, want %d", len(seen), pages, total)
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("vm %s missing from paginated results", id)
		}
	}
	if wantPages := (total + pageSize - 1) / pageSize; pages != wantPages {
		t.Errorf("pages = %d, want %d", pages, wantPages)
	}
}

// TestListFleetFiltered_limitClamping checks the default/max page size
// contract: <=0 becomes DefaultPageLimit, and anything above MaxPageLimit
// is clamped down rather than rejected.
func TestListFleetFiltered_limitClamping(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()

	for i := 0; i < DefaultPageLimit+10; i++ {
		taskID := fmt.Sprintf("task-%03d", i)
		if _, err := fm.ProvisionAndAssign(ctx, taskID, Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
	}

	page, next, err := fm.ListFleetFiltered(VMFilter{}) // limit unset
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != DefaultPageLimit {
		t.Fatalf("unset limit returned %d, want default %d", len(page), DefaultPageLimit)
	}
	if next == "" {
		t.Fatal("expected a next cursor since more results remain")
	}

	page, _, err = fm.ListFleetFiltered(VMFilter{Limit: MaxPageLimit + 1000})
	if err != nil {
		t.Fatalf("list with oversized limit: %v", err)
	}
	if len(page) != DefaultPageLimit+10 {
		t.Fatalf("oversized limit returned %d, want all %d (clamped to %d > total)", len(page), DefaultPageLimit+10, MaxPageLimit)
	}
}

// TestListFleetFiltered_invalidCursorErrors checks a malformed cursor
// surfaces ErrInvalidCursor rather than panicking or silently ignoring it.
func TestListFleetFiltered_invalidCursorErrors(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})

	if _, _, err := fm.ListFleetFiltered(VMFilter{Cursor: "not-a-valid-cursor!!"}); err == nil {
		t.Fatal("expected an error for a malformed cursor")
	}
}

// TestListSnapshotsFiltered_paginatesAndRoundTripsCursor mirrors the VM
// pagination test for snapshots, and checks the page ordering matches
// sortSnapshotRecords (newest first).
func TestListSnapshotsFiltered_paginatesAndRoundTripsCursor(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	ctx := context.Background()
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	const total = 60
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		rec, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{Comment: fmt.Sprintf("snap-%d", i)})
		if err != nil {
			t.Fatalf("create snapshot %d: %v", i, err)
		}
		created[rec.SnapshotID] = true
	}

	const pageSize = 20
	seen := make(map[string]bool, total)
	var last SnapshotRecord
	haveLast := false
	cursor := ""
	pages := 0
	for {
		page, next, err := fm.ListSnapshotsFiltered(ctx, SnapshotFilter{Limit: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > total {
			t.Fatalf("did not terminate after %d pages", pages)
		}
		for _, s := range page {
			if seen[s.SnapshotID] {
				t.Fatalf("snapshot %s returned twice across pages", s.SnapshotID)
			}
			seen[s.SnapshotID] = true
			if haveLast && !snapshotRecordLess(last, s) {
				t.Fatalf("page order violated sortSnapshotRecords: %s then %s", last.SnapshotID, s.SnapshotID)
			}
			last, haveLast = s, true
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != total {
		t.Fatalf("saw %d unique snapshots across %d pages, want %d", len(seen), pages, total)
	}
	for id := range created {
		if !seen[id] {
			t.Errorf("snapshot %s missing from paginated results", id)
		}
	}
}

// TestListHostsFiltered_paginatesAndRoundTripsCursor mirrors the VM
// pagination test for hosts.
func TestListHostsFiltered_paginatesAndRoundTripsCursor(t *testing.T) {
	stub := newStubProvider()
	fm := NewFleetManager(FleetConfig{Provider: stub, Prefix: "fuse-"})
	ctx := context.Background()

	const total = 30
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("host-%03d", i)
		if err := fm.RegisterHost(ctx, Host{ID: id, URL: "http://" + id}, stub); err != nil {
			t.Fatalf("register host %d: %v", i, err)
		}
		want[id] = true
	}

	const pageSize = 7
	seen := make(map[string]bool, total)
	var lastID string
	cursor := ""
	pages := 0
	for {
		page, next, err := fm.ListHostsFiltered(HostFilter{Limit: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > total {
			t.Fatalf("did not terminate after %d pages", pages)
		}
		for _, h := range page {
			if seen[h.ID] {
				t.Fatalf("host %s returned twice across pages", h.ID)
			}
			seen[h.ID] = true
			if lastID != "" && h.ID <= lastID {
				t.Fatalf("ids not ascending across pages: %s then %s", lastID, h.ID)
			}
			lastID = h.ID
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != total {
		t.Fatalf("saw %d unique hosts across %d pages, want %d", len(seen), pages, total)
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("host %s missing from paginated results", id)
		}
	}
}

// TestListFleet_autoPaginatesBeyondOnePage checks the ListFleet()
// convenience wrapper (used internally and by tests wanting "everything")
// still returns the full set once results exceed one page.
func TestListFleet_autoPaginatesBeyondOnePage(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()

	const total = DefaultPageLimit + 15
	for i := 0; i < total; i++ {
		taskID := fmt.Sprintf("task-%03d", i)
		if _, err := fm.ProvisionAndAssign(ctx, taskID, Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
	}

	fleet := fm.ListFleet()
	if len(fleet) != total {
		t.Fatalf("ListFleet returned %d, want %d", len(fleet), total)
	}
}

// TestCreateSnapshot_quotaUsesCountNotFullScan exercises the same behavior
// TestSnapshot_quotaExceededReturns409 (api package) checks over HTTP: the
// per-tenant quota is enforced via the state store's counted
// SnapshotQuotaUsage rather than by loading every snapshot in the fleet.
// This pins the orchestrator-level contract directly.
func TestCreateSnapshot_quotaUsesCountNotFullScan(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider:              provider,
		Prefix:                "fuse-",
		SnapshotQuotaMaxCount: 2,
	})
	ctx := context.Background()
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	if _, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{}); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if _, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{}); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if _, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{}); err == nil {
		t.Fatal("expected third snapshot to trip the quota")
	} else if err.Error() == "" {
		t.Fatal("expected a quota error message")
	}

	usage, err := fm.store.SnapshotQuotaUsage(ctx, "task-1")
	if err != nil {
		t.Fatalf("SnapshotQuotaUsage: %v", err)
	}
	if usage.Count != 2 {
		t.Fatalf("usage.Count = %d, want 2", usage.Count)
	}
}
