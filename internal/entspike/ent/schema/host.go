// Package schema is an ent spike: mirrors a subset of the real orchestrator
// tables (hosts, vms) to evaluate ent before adopting it in the state store.
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Host mirrors orchestrator_hosts.
type Host struct {
	ent.Schema
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
		field.Int("cpus_total").Default(0),
		field.Int("ram_mb_total").Default(0),
		field.Int("storage_gb_total").Default(0),
		field.Int("vm_count_max").Default(0),
		field.Int("cpus_allocated").Default(0),
		field.Int("ram_mb_allocated").Default(0),
		field.Int("storage_gb_allocated").Default(0),
		field.Int("vm_count_allocated").Default(0),
		// operator-declared labels, open-ended key set
		field.JSON("labels", map[string]string{}).Default(map[string]string{}),
		field.String("arch").Default(""),
		// added after the baseline to test alter generation
		field.String("zone").Default(""),
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
