-- Host cpu architecture in goarch vocabulary ("amd64", "arm64"), probed from
-- the host agent at registration (or operator-declared). The scheduler treats
-- '' as amd64: every host registered before this column existed was x86_64,
-- so unmigrated rows stay schedulable without a backfill.
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT '';
