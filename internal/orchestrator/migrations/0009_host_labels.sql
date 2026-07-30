-- Operator-declared host labels (issue #102): key/value pairs supplied at
-- registration and matched against a Fusefile's placement.labels selector.
-- A JSON object column ('{}' = no labels) because the key set is open-ended,
-- like mig_profiles_json and unlike the fixed region/backend scalars. Labels
-- are declared, never probed, the same trust model as gpu_kind.
ALTER TABLE orchestrator_hosts ADD COLUMN IF NOT EXISTS labels_json TEXT NOT NULL DEFAULT '{}';
