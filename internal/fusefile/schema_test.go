package fusefile

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaPath is the published json schema for the authoring format.
//
// the schema is hand-maintained rather than generated: most of its value is in
// rules a generator could not derive from struct tags (the size pattern, the
// MIG profile form, value/secret exclusivity), and generating it would put a
// json schema dependency in a go.mod that has none. this test is what pays for
// that choice, so a new Fusefile field cannot merge without a schema update.
const schemaPath = "../../schema/fusefile-v1.json"

// schemaID must match the modeline written by `fuse init` and documented in
// docs/. changing it invalidates every modeline already committed to a user's
// repo, so a v2 gets a new file at a new url instead.
const schemaID = "https://raw.githubusercontent.com/folsomintel/fuse/main/schema/fusefile-v1.json"

// TestFusefileSchemaInSync walks the Fusefile struct tree and the json schema
// together and fails on any divergence in either direction: a yaml tag with no
// schema property, or a schema property with no yaml tag.
//
// it checks the field set only. the semantic constraints (patterns, enums,
// oneOf) are the schema's own business, and Go stays authoritative for them.
func TestFusefileSchemaInSync(t *testing.T) {
	doc := loadSchema(t)
	doc.checkStruct(t, "", reflect.TypeOf(Fusefile{}), doc.root)
}

// TestFusefileSchemaID pins the published url. see the schemaID comment.
func TestFusefileSchemaID(t *testing.T) {
	doc := loadSchema(t)
	if got := doc.root["$id"]; got != schemaID {
		t.Errorf("$id = %v, want %s", got, schemaID)
	}
	if got := doc.root["properties"].(map[string]any)["version"].(map[string]any)["const"]; got != float64(1) {
		t.Errorf("version const = %v, want 1", got)
	}
}

// schemaDoc is a parsed schema plus the ref resolution the walk needs.
type schemaDoc struct {
	root map[string]any
}

func loadSchema(t *testing.T) *schemaDoc {
	t.Helper()

	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("schema is not well-formed json: %v", err)
	}
	return &schemaDoc{root: root}
}

// resolve follows a local "#/$defs/name" reference to the node it names.
func (d *schemaDoc) resolve(t *testing.T, path string, node map[string]any) map[string]any {
	t.Helper()

	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		t.Fatalf("%s: only local %s refs are supported, got %q", path, prefix, ref)
	}
	defs, _ := d.root["$defs"].(map[string]any)
	target, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	if !ok {
		t.Fatalf("%s: $ref %q does not resolve", path, ref)
	}
	return target
}

// objectBranch returns the node that carries "properties". a node written as a
// oneOf (setup steps accept a bare string or a mapping) carries them on one
// branch, so unwrap to it.
func (d *schemaDoc) objectBranch(t *testing.T, path string, node map[string]any) map[string]any {
	t.Helper()

	node = d.resolve(t, path, node)
	if _, ok := node["properties"]; ok {
		return node
	}
	branches, _ := node["oneOf"].([]any)
	for _, branch := range branches {
		b, ok := branch.(map[string]any)
		if !ok {
			continue
		}
		if b = d.resolve(t, path, b); b["properties"] != nil {
			return b
		}
	}
	return node
}

// checkStruct compares one struct's yaml tags against one schema node's
// properties, then recurses through every field.
func (d *schemaDoc) checkStruct(t *testing.T, path string, typ reflect.Type, node map[string]any) {
	t.Helper()

	node = d.objectBranch(t, path, node)
	props, ok := node["properties"].(map[string]any)
	if !ok {
		t.Errorf("%s: schema node has no properties, but %s is a struct", label(path), typ)
		return
	}

	fields := yamlFields(typ)

	for _, name := range sorted(fields) {
		if _, ok := props[name]; !ok {
			t.Errorf("%s: yaml field %q has no schema property; add it to %s", label(path), name, schemaPath)
		}
	}
	for _, name := range sorted(props) {
		if _, ok := fields[name]; !ok {
			t.Errorf("%s: schema property %q has no yaml field; remove it from %s", label(path), name, schemaPath)
		}
	}

	for name, ft := range fields {
		prop, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		d.checkField(t, path+"."+name, ft, prop)
	}
}

// checkField recurses into whatever the field's type eventually contains: a
// slice descends into "items", a map into "additionalProperties", and a struct
// gets compared. anything else (string, int, bool) is a leaf. A struct with no
// yaml-tagged fields is also a leaf: it models a polymorphic scalar (e.g.
// Command's shell-vs-argv) whose yaml shape is owned by a custom unmarshaler,
// not by object fields the schema would have to describe.
func (d *schemaDoc) checkField(t *testing.T, path string, typ reflect.Type, node map[string]any) {
	t.Helper()

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	// Opaque: a custom type with no yaml-tagged fields carries no field-level
	// contract for the schema to mirror, so there is nothing to walk. This
	// does not suppress the reverse parity check in checkStruct, which still
	// flags any schema property a struct claims but does not back with a
	// field. (No fielded struct in the tree reaches this branch: every
	// current struct type carries yaml tags.)
	if typ.Kind() == reflect.Struct && len(yamlFields(typ)) == 0 {
		return
	}

	switch typ.Kind() {
	case reflect.Struct:
		d.checkStruct(t, path, typ, node)
	case reflect.Slice:
		items, ok := d.resolve(t, path, node)["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: schema node has no items, but the field is a list", label(path))
			return
		}
		d.checkField(t, path+"[]", typ.Elem(), items)
	case reflect.Map:
		extra, ok := d.resolve(t, path, node)["additionalProperties"].(map[string]any)
		if !ok {
			t.Errorf("%s: schema node has no additionalProperties, but the field is a map", label(path))
			return
		}
		d.checkField(t, path+".*", typ.Elem(), extra)
	}
}

// yamlFields maps a struct's yaml tag names to their field types, skipping
// unexported and explicitly ignored fields.
func yamlFields(typ reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = field.Type
	}
	return out
}

// label renders a path for an error message; the root has no path of its own.
func label(path string) string {
	if path == "" {
		return "(root)"
	}
	return strings.TrimPrefix(path, ".")
}

// sorted returns a map's keys in order so failures read the same every run.
func sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSchemaDescribesParseableFusefile guards the example carried in the docs
// and in the schema's own description: the modeline is a yaml comment, so a
// schema-aware Fusefile must still parse unchanged.
func TestSchemaDescribesParseableFusefile(t *testing.T) {
	src := fmt.Sprintf("# yaml-language-server: $schema=%s\nversion: 1\nresources:\n  cpus: 2\n  memory: 2GB\nrun: ./start.sh\n", schemaID)
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("the documented modeline example does not parse: %v", err)
	}
}

// TestCommandMarshalRoundTrip verifies that Command's custom (un)marshaler
// preserves each form byte-for-byte and that an unset `run` is omitted by
// omitempty rather than serialized as an empty scalar. This is the contract the
// `fuse init` scaffold and any future `fuse validate --print` rely on.
func TestCommandMarshalRoundTrip(t *testing.T) {
	t.Run("shell form round-trips", func(t *testing.T) {
		f := Fusefile{Version: 1, Run: Command{Shell: "./serve.sh --port 8080"}}
		out, err := yaml.Marshal(&f)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := Parse(out)
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		if got.Run.Shell != f.Run.Shell || got.Run.Argv != nil {
			t.Errorf("Round-trip Run = %+v, want %+v", got.Run, f.Run)
		}
	})

	t.Run("exec form round-trips", func(t *testing.T) {
		f := Fusefile{Version: 1, Run: Command{Argv: []string{"python", "it's hot.py"}}}
		out, err := yaml.Marshal(&f)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := Parse(out)
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		if !reflect.DeepEqual(got.Run, f.Run) {
			t.Errorf("Round-trip Run = %+v, want %+v", got.Run, f.Run)
		}
	})

	t.Run("empty command is omitted", func(t *testing.T) {
		out, err := yaml.Marshal(&Fusefile{Version: 1})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), "run") {
			t.Errorf("marshal(empty Fusefile) emitted a run line:\n%s", out)
		}
	})
}
