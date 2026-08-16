// Package schema is an ent spike: mirrors a subset of the real orchestrator
// tables (hosts, vms) to evaluate ent before adopting it in the state store.
// Column types, storage keys, and table names are pinned to match the
// hand-written migrations in internal/orchestrator/migrations exactly, so
// the same schema works against a database created by ApplyMigrations.
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// pgInt pins ent's default bigint mapping to INTEGER, matching the
// hand-written schema.
var pgInt = map[string]string{dialect.Postgres: "integer"}

// Host mirrors orchestrator_hosts.
type Host struct {
	ent.Schema
}

func (Host) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "orchestrator_hosts"},
	}
}

func (Host) Fields() []ent.Field {
	return []ent.Field{
		// text primary key, same as host_id in the real table
		field.String("id").StorageKey("host_id"),
		field.String("url"),
		// agent bearer token, encrypted at rest (bytea)
		field.Bytes("token_encrypted").Optional(),
		field.String("region").Default(""),
		field.Enum("state").Values("active", "cordoned").Default("active"),
		field.String("tenant_id").Default(""),
		field.Int("cpus_total").Default(0).SchemaType(pgInt),
		field.Int("ram_mb_total").Default(0).SchemaType(pgInt),
		field.Int("storage_gb_total").Default(0).SchemaType(pgInt),
		field.Int("vm_count_max").Default(0).SchemaType(pgInt),
		field.Int("cpus_allocated").Default(0).SchemaType(pgInt),
		field.Int("ram_mb_allocated").Default(0).SchemaType(pgInt),
		field.Int("storage_gb_allocated").Default(0).SchemaType(pgInt),
		field.Int("vm_count_allocated").Default(0).SchemaType(pgInt),
		// operator-declared labels; stored as a json object in a text
		// column (labels_json), same as migration 0009
		field.JSON("labels", map[string]string{}).
			StorageKey("labels_json").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(map[string]string{}),
		field.String("arch").Default(""),
		field.Time("last_seen_at"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Host) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("state"),
		index.Fields("region"),
		index.Fields("tenant_id"),
	}
}
