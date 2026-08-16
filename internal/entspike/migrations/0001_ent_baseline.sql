-- create "hosts" table
CREATE TABLE "hosts" ("host_id" character varying NOT NULL, "url" character varying NOT NULL, "token_encrypted" bytea NULL, "region" character varying NOT NULL DEFAULT '', "state" character varying NOT NULL DEFAULT 'active', "tenant_id" character varying NOT NULL DEFAULT '', "cpus_total" bigint NOT NULL DEFAULT 0, "ram_mb_total" bigint NOT NULL DEFAULT 0, "storage_gb_total" bigint NOT NULL DEFAULT 0, "vm_count_max" bigint NOT NULL DEFAULT 0, "cpus_allocated" bigint NOT NULL DEFAULT 0, "ram_mb_allocated" bigint NOT NULL DEFAULT 0, "storage_gb_allocated" bigint NOT NULL DEFAULT 0, "vm_count_allocated" bigint NOT NULL DEFAULT 0, "labels" jsonb NOT NULL, "arch" character varying NOT NULL DEFAULT '', "last_seen_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("host_id"));
-- create index "host_region" to table: "hosts"
CREATE INDEX "host_region" ON "hosts" ("region");
-- create index "host_state" to table: "hosts"
CREATE INDEX "host_state" ON "hosts" ("state");
-- create index "host_tenant_id" to table: "hosts"
CREATE INDEX "host_tenant_id" ON "hosts" ("tenant_id");
-- create "vms" table
CREATE TABLE "vms" ("vm_id" character varying NOT NULL, "host_id" character varying NOT NULL DEFAULT '', "network_host" character varying NOT NULL DEFAULT '', "state" character varying NOT NULL, "url" character varying NOT NULL DEFAULT '', "task_id" character varying NOT NULL DEFAULT '', "tenant_id" character varying NOT NULL DEFAULT '', "cpus" bigint NOT NULL DEFAULT 0, "ram_mb" bigint NOT NULL DEFAULT 0, "storage_gb" bigint NOT NULL DEFAULT 0, "region" character varying NOT NULL DEFAULT '', "max_runtime_seconds" bigint NOT NULL DEFAULT 0, "auth_token_encrypted" bytea NULL, "secrets_encrypted" bytea NULL, "last_error" character varying NOT NULL DEFAULT '', "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("vm_id"));
-- create index "vm_host_id" to table: "vms"
CREATE INDEX "vm_host_id" ON "vms" ("host_id");
-- create index "vm_state" to table: "vms"
CREATE INDEX "vm_state" ON "vms" ("state");
-- create index "vm_task_id" to table: "vms"
CREATE INDEX "vm_task_id" ON "vms" ("task_id");
-- create index "vm_tenant_id" to table: "vms"
CREATE INDEX "vm_tenant_id" ON "vms" ("tenant_id");
