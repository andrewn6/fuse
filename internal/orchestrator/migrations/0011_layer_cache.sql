-- Layer cache identity for build artifacts. layer_key is the derived
-- content-addressed key of one setup step (see internal/fusefile/layer.go),
-- arch is the goarch of the host that actually built the artifact, and digest
-- is the hex sha256 of the artifact rootfs. Lookups only ever match a
-- non-empty layer_key, so every snapshot written before this column existed
-- stays correct as '' without a backfill: '' is not a key anyone can derive.
ALTER TABLE orchestrator_snapshots ADD COLUMN IF NOT EXISTS layer_key TEXT NOT NULL DEFAULT '';
-- Recorded from the building host rather than folded into layer_key: an ext4
-- rootfs is not portable across architectures, but the key is derived
-- client-side before any host is scheduled, so arch is a serving filter and
-- never a key component.
ALTER TABLE orchestrator_snapshots ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT '';
-- An integrity check on these exact bytes, never a cross-build identity: two
-- builds of the same recipe produce different rootfs bytes (timestamps, inode
-- ordering, package caches), so digest can never be a cache key and dedup is
-- not a goal here. Nothing looks a snapshot up by digest.
ALTER TABLE orchestrator_snapshots ADD COLUMN IF NOT EXISTS digest TEXT NOT NULL DEFAULT '';

-- The lookup index. Deliberately NOT unique: two concurrent builds of the same
-- recipe both legitimately insert an artifact, and failing one of them would
-- turn a cache into a lock. Duplicates are resolved at read time instead, by
-- created_at DESC, so the newest ready artifact wins. tenant_id leads because
-- a layer is never served across tenants. The partial predicate keeps every
-- non-layer snapshot (manual/auto checkpoints, and build artifacts predating
-- this migration) out of the index entirely.
CREATE INDEX IF NOT EXISTS orchestrator_snapshots_layer_key_idx
    ON orchestrator_snapshots (tenant_id, layer_key, arch, created_at DESC)
    WHERE layer_key <> '';
