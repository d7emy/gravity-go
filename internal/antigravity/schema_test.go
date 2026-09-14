package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseSchema is a helper so tests can write schemas as JSON literals.
func parseSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("bad test schema: %v", err)
	}
	return out
}

func cleanToJSON(t *testing.T, raw string) (map[string]any, string) {
	t.Helper()
	cleaned, ok := CleanJSONSchemaForGemini(parseSchema(t, raw)).(map[string]any)
	if !ok {
		t.Fatalf("cleaner did not return an object")
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		t.Fatalf("cleaned schema is not encodable: %v", err)
	}
	return cleaned, string(encoded)
}

// dig walks a nested map by key path.
func dig(t *testing.T, node map[string]any, path ...string) map[string]any {
	t.Helper()
	current := node
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("path %v missing at %q", path, key)
		}
		current = next
	}
	return current
}

func TestInlinesRefFromDefs(t *testing.T) {
	cleaned, encoded := cleanToJSON(t, `{
		"type": "object",
		"properties": {"user": {"$ref": "#/$defs/User"}},
		"$defs": {"User": {"type": "object", "properties": {"name": {"type": "string"}}}}
	}`)

	if _, present := cleaned["$defs"]; present {
		t.Error("$defs should be stripped after inlining")
	}
	if strings.Contains(encoded, "$ref") {
		t.Error("$ref survived into the cleaned schema")
	}

	user := dig(t, cleaned, "properties", "user")
	if user["type"] != "object" {
		t.Errorf("user type = %v, want object", user["type"])
	}
	name := dig(t, cleaned, "properties", "user", "properties", "name")
	if name["type"] != "string" {
		t.Errorf("user.name type = %v, want string", name["type"])
	}
}

// A tree node whose children are the same type is exactly what MCP servers
// produce. Without the depth cap this expands forever.
func TestSelfReferencingSchemaTerminates(t *testing.T) {
	cleaned, encoded := cleanToJSON(t, `{
		"type": "object",
		"properties": {"root": {"$ref": "#/$defs/Node"}},
		"$defs": {
			"Node": {
				"type": "object",
				"properties": {
					"value": {"type": "string"},
					"children": {"type": "array", "items": {"$ref": "#/$defs/Node"}}
				}
			}
		}
	}`)

	root := dig(t, cleaned, "properties", "root")
	if root["type"] != "object" {
		t.Errorf("root type = %v, want object", root["type"])
	}
	value := dig(t, cleaned, "properties", "root", "properties", "value")
	if value["type"] != "string" {
		t.Errorf("root.value type = %v, want string", value["type"])
	}
	if strings.Contains(encoded, "$ref") {
		t.Error("$ref survived a recursive definition")
	}
}

func TestMutuallyRecursiveDefinitionsTerminate(t *testing.T) {
	cleaned, encoded := cleanToJSON(t, `{
		"type": "object",
		"properties": {"a": {"$ref": "#/$defs/A"}},
		"$defs": {
			"A": {"type": "object", "properties": {"b": {"$ref": "#/$defs/B"}}},
			"B": {"type": "object", "properties": {"a": {"$ref": "#/$defs/A"}}}
		}
	}`)

	a := dig(t, cleaned, "properties", "a")
	if a["type"] != "object" {
		t.Errorf("a type = %v, want object", a["type"])
	}
	if strings.Contains(encoded, "$ref") {
		t.Error("$ref survived mutual recursion")
	}
}

// An empty node tells the model nothing; a permissive object keeps the
// parameter usable.
func TestUnresolvableRefBecomesPermissiveObject(t *testing.T) {
	cleaned, _ := cleanToJSON(t, `{
		"type": "object",
		"properties": {"thing": {"$ref": "https://example.com/external.json#/Thing"}}
	}`)

	thing := dig(t, cleaned, "properties", "thing")
	if _, present := thing["$ref"]; present {
		t.Error("$ref should be removed")
	}
	if thing["type"] != "object" {
		t.Errorf("thing type = %v, want object", thing["type"])
	}
}

func TestStrayRefNeverReachesUpstream(t *testing.T) {
	cleaned, encoded := cleanToJSON(t, `{
		"type": "object",
		"properties": {"x": {"$ref": "#/$defs/Missing", "description": "keep me"}}
	}`)

	if strings.Contains(encoded, "$ref") {
		t.Error("$ref survived with no $defs present")
	}
	x := dig(t, cleaned, "properties", "x")
	description, _ := x["description"].(string)
	if !strings.Contains(description, "keep me") {
		t.Errorf("description = %q, want it to retain the original text", description)
	}
}

func TestHardRemovedFieldsAreStripped(t *testing.T) {
	_, encoded := cleanToJSON(t, `{
		"type": "object",
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"additionalProperties": false,
		"properties": {"a": {"type": "string", "format": "email", "default": "x@y.z"}}
	}`)

	for _, field := range []string{"$schema", "additionalProperties", "format", "default"} {
		if strings.Contains(encoded, field) {
			t.Errorf("%q should have been removed, got %s", field, encoded)
		}
	}
}

// Validation keywords are folded into the description so the model still sees
// the constraint Gemini will not enforce.
func TestValidationFieldsMigrateToDescription(t *testing.T) {
	cleaned, _ := cleanToJSON(t, `{
		"type": "object",
		"properties": {"name": {"type": "string", "minLength": 3, "pattern": "^a"}}
	}`)

	name := dig(t, cleaned, "properties", "name")
	description, _ := name["description"].(string)
	if !strings.Contains(description, "Constraint:") {
		t.Fatalf("description = %q, want a Constraint clause", description)
	}
	if !strings.Contains(description, "minLength: 3") {
		t.Errorf("description = %q, want minLength preserved as an integer", description)
	}
	if !strings.Contains(description, "pattern: ^a") {
		t.Errorf("description = %q, want pattern preserved", description)
	}
	if _, present := name["minLength"]; present {
		t.Error("minLength should have moved out of the schema")
	}
}

func TestUnionTypeCollapsesToFirstNonNull(t *testing.T) {
	cleaned, _ := cleanToJSON(t, `{
		"type": "object",
		"properties": {"maybe": {"type": ["string", "null"]}}
	}`)

	maybe := dig(t, cleaned, "properties", "maybe")
	if maybe["type"] != "string" {
		t.Errorf("type = %v, want string", maybe["type"])
	}
}

func TestAnyOfYieldsTypeThenIsRemoved(t *testing.T) {
	cleaned, encoded := cleanToJSON(t, `{
		"type": "object",
		"properties": {"v": {"anyOf": [{"type": "null"}, {"type": "number"}]}}
	}`)

	v := dig(t, cleaned, "properties", "v")
	if v["type"] != "number" {
		t.Errorf("type = %v, want number salvaged from anyOf", v["type"])
	}
	if strings.Contains(encoded, "anyOf") {
		t.Error("anyOf should be removed after the type is salvaged")
	}
}

func TestRequiredIsFilteredToExistingProperties(t *testing.T) {
	cleaned, _ := cleanToJSON(t, `{
		"type": "object",
		"properties": {"a": {"type": "string"}},
		"required": ["a", "ghost"]
	}`)

	required, ok := cleaned["required"].([]any)
	if !ok {
		t.Fatalf("required = %v, want an array", cleaned["required"])
	}
	if len(required) != 1 || required[0] != "a" {
		t.Errorf("required = %v, want [a]", required)
	}
}

func TestEnumValuesBecomeStrings(t *testing.T) {
	cleaned, _ := cleanToJSON(t, `{
		"type": "object",
		"properties": {"mode": {"type": "string", "enum": ["a", 2, true, null]}}
	}`)

	mode := dig(t, cleaned, "properties", "mode")
	enum, ok := mode["enum"].([]any)
	if !ok {
		t.Fatalf("enum = %v, want an array", mode["enum"])
	}
	want := []string{"a", "2", "true", "null"}
	for i, expected := range want {
		if enum[i] != expected {
			t.Errorf("enum[%d] = %v, want %q", i, enum[i], expected)
		}
	}
}

func TestNormalizeToolParametersHandlesJunk(t *testing.T) {
	cases := []struct {
		name  string
		input any
	}{
		{"nil", nil},
		{"non-schema string", "not json"},
		{"array", []any{1, 2}},
		{"number", 42.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := NormalizeToolParameters(tc.input)
			if out["type"] != "object" {
				t.Errorf("type = %v, want object", out["type"])
			}
			if _, ok := out["properties"].(map[string]any); !ok {
				t.Errorf("properties = %v, want an object", out["properties"])
			}
		})
	}
}

func TestNormalizeToolParametersAcceptsJSONString(t *testing.T) {
	out := NormalizeToolParameters(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	properties, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want an object", out["properties"])
	}
	if _, present := properties["q"]; !present {
		t.Errorf("properties = %v, want q preserved from the JSON string form", properties)
	}
}

// The cleaner must never mutate the caller's schema: the same tool definition is
// reused across every request in a session.
func TestCleanerDoesNotMutateInput(t *testing.T) {
	original := parseSchema(t, `{
		"type": "object",
		"properties": {"a": {"type": "string", "format": "email"}},
		"$defs": {"X": {"type": "string"}}
	}`)
	before, _ := json.Marshal(original)

	CleanJSONSchemaForGemini(original)

	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Errorf("input was mutated:\n before %s\n after  %s", before, after)
	}
}
