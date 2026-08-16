package schema

import (
	"encoding/json"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

var pgText = map[string]string{dialect.Postgres: "text"}

// VM mirrors orchestrator_vms, full column set. host_id stays a loose
// string reference (no ent edge) to match the repo convention: no FK
// constraints, cascade behaviour is owned by FleetManager in code, not
// the database.
//
// The three json columns stay json.RawMessage rather than typed slices so
// the store keeps the exact marshal/unmarshal semantics of the hand-written
// implementation (nil coercion, empty-vs-null distinctions) and the ent
// packages never import internal/orchestrator (which imports them back).
//
// created_at/updated_at have no defaults and no Immutable: the store sets
// both explicitly and the upsert overwrites them, same as the EXCLUDED
// assignments in the original sql.
type VM struct {
	ent.Schema
}

func (VM) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "orchestrator_vms"},
	}
}

func (VM) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("vm_id"),
		field.String("host_id").Default(""),
		field.String("network_host").Default(""),
		field.Enum("state").Values(
			"provisioning", "running", "draining", "destroying",
			// terminal states written by the event path
			"destroyed", "failed",
		),
		field.String("url").Default(""),
		field.String("task_id").Default(""),
		field.String("tenant_id").Default(""),
		field.Int("cpus").Default(0).SchemaType(pgInt),
		field.Int("ram_mb").Default(0).SchemaType(pgInt),
		field.Int("storage_gb").Default(0).SchemaType(pgInt),
		field.String("region").Default(""),
		field.Int("max_runtime_seconds").Default(0).SchemaType(pgInt),
		field.Int("idle_timeout_seconds").Default(0).SchemaType(pgInt),
		field.Bytes("auth_token_encrypted").Optional(),
		field.Bytes("secrets_encrypted").Optional(),
		field.String("last_error").Default(""),
		field.JSON("endpoints", json.RawMessage{}).
			StorageKey("endpoints_json").Optional().
			SchemaType(pgText),
		field.Int32("gpus").Default(0).SchemaType(pgInt),
		field.String("gpu_kind").Default(""),
		field.String("gpu_profile").Default(""),
		field.JSON("gpu_uuids", json.RawMessage{}).Optional().
			SchemaType(pgText),
		field.JSON("mig_instance_uuids", json.RawMessage{}).Optional().
			SchemaType(pgText),
		field.Time("created_at"),
		field.Time("updated_at"),
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
