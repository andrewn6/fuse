-- create "orchestrator_hosts" table
CREATE TABLE "orchestrator_hosts" ("host_id" character varying NOT NULL, "url" character varying NOT NULL, "token_encrypted" bytea NULL, "region" character varying NOT NULL DEFAULT '', "state" character varying NOT NULL DEFAULT 'active', "tenant_id" character varying NOT NULL DEFAULT '', "cpus_total" integer NOT NULL DEFAULT 0, "ram_mb_total" integer NOT NULL DEFAULT 0, "storage_gb_total" integer NOT NULL DEFAULT 0, "vm_count_max" integer NOT NULL DEFAULT 0, "cpus_allocated" integer NOT NULL DEFAULT 0, "ram_mb_allocated" integer NOT NULL DEFAULT 0, "storage_gb_allocated" integer NOT NULL DEFAULT 0, "vm_count_allocated" integer NOT NULL DEFAULT 0, "labels_json" text NOT NULL, "arch" character varying NOT NULL DEFAULT '', "last_seen_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("host_id"));
-- create index "host_region" to table: "orchestrator_hosts"
CREATE INDEX "host_region" ON "orchestrator_hosts" ("region");
-- create index "host_state" to table: "orchestrator_hosts"
CREATE INDEX "host_state" ON "orchestrator_hosts" ("state");
-- create index "host_tenant_id" to table: "orchestrator_hosts"
CREATE INDEX "host_tenant_id" ON "orchestrator_hosts" ("tenant_id");
-- create "orchestrator_vms" table
CREATE TABLE "orchestrator_vms" ("vm_id" character varying NOT NULL, "host_id" character varying NOT NULL DEFAULT '', "network_host" character varying NOT NULL DEFAULT '', "state" character varying NOT NULL, "url" character varying NOT NULL DEFAULT '', "task_id" character varying NOT NULL DEFAULT '', "tenant_id" character varying NOT NULL DEFAULT '', "cpus" integer NOT NULL DEFAULT 0, "ram_mb" integer NOT NULL DEFAULT 0, "storage_gb" integer NOT NULL DEFAULT 0, "region" character varying NOT NULL DEFAULT '', "max_runtime_seconds" integer NOT NULL DEFAULT 0, "idle_timeout_seconds" integer NOT NULL DEFAULT 0, "auth_token_encrypted" bytea NULL, "secrets_encrypted" bytea NULL, "last_error" character varying NOT NULL DEFAULT '', "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("vm_id"));
-- create index "vm_host_id" to table: "orchestrator_vms"
CREATE INDEX "vm_host_id" ON "orchestrator_vms" ("host_id");
-- create index "vm_state" to table: "orchestrator_vms"
CREATE INDEX "vm_state" ON "orchestrator_vms" ("state");
-- create index "vm_task_id" to table: "orchestrator_vms"
CREATE INDEX "vm_task_id" ON "orchestrator_vms" ("task_id");
-- create index "vm_tenant_id" to table: "orchestrator_vms"
CREATE INDEX "vm_tenant_id" ON "orchestrator_vms" ("tenant_id");
