-- per-vm idle timeout: how long a running vm may go with no exec and no
-- attach session before the reconcile loop tears it down. distinct from
-- max_runtime_seconds, which is a leak-detection ceiling measured from create.
--
-- additive + back-compat: existing rows default to 0, which means "no idle
-- expiry" (or the fleet default when one is configured).
ALTER TABLE orchestrator_vms ADD COLUMN IF NOT EXISTS idle_timeout_seconds INTEGER NOT NULL DEFAULT 0;
