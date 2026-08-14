-- The artifact index: "which hosts already hold these exact bytes".
--
-- There is no separate index table on purpose. A snapshot row already says
-- "host H holds artifact A", so a second store of the same fact could only ever
-- drift from it, and the drift would stay invisible until a peer pull was aimed
-- at a host that no longer had the blob. Deriving the mapping from the rows
-- also makes orchestrator recovery free: there is nothing to rehydrate, because
-- the query IS the index.
--
-- The partial predicate keeps every snapshot taken by an agent build that
-- predates artifact hashing (digest '') out of the index entirely. Those rows
-- must never read as "this host holds artifact ''" -- that would name every
-- such host as a source peer for a blob none of them has.
--
-- Note what this index is NOT for: nothing dedups on digest. Two builds of the
-- same recipe produce different rootfs bytes (timestamps, inode ordering,
-- package caches), so a digest is an integrity check on one artifact's bytes
-- and never a cross-build identity. Rows sharing a digest are copies of one
-- artifact, which is exactly what a peer pull creates.
CREATE INDEX IF NOT EXISTS orchestrator_snapshots_digest_idx
    ON orchestrator_snapshots (tenant_id, digest, snapshot_id)
    WHERE digest <> '';
