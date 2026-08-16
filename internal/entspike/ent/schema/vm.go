package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// VM mirrors orchestrator_vms. host_id stays a loose string reference
// (no ent edge) to match the repo convention: no FK constraints, cascade
// behaviour is owned by FleetManager in code, not the database.
type VM struct {
	ent.Schema
}

func (VM) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("vm_id"),
		field.String("host_id").Default(""),
		field.String("network_host").Default(""),
		field.Enum("state").Values("provisioning", "running", "draining", "destroying"),
		field.String("url").Default(""),
		field.String("task_id").Default(""),
		field.String("tenant_id").Default(""),
		field.Int("cpus").Default(0),
		field.Int("ram_mb").Default(0),
		field.Int("storage_gb").Default(0),
		field.String("region").Default(""),
		field.Int("max_runtime_seconds").Default(0),
		field.Bytes("auth_token_encrypted").Optional(),
		field.Bytes("secrets_encrypted").Optional(),
		field.String("last_error").Default(""),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (VM) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("state"),
		index.Fields("task_id"),
		index.Fields("host_id"),
		index.Fields("tenant_id"),
	}
}
