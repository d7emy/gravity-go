package antigravity

import (
	"encoding/json"
	"fmt"
	"testing"
)

// agentToolSchema is a tool definition of the size a coding agent actually
// sends: nested objects, enums, a $ref/$defs pair and a description on every
// field. An agent resends its whole tool set on every single request, so this
// runs on the hot path once per tool per turn.
func agentToolSchema() any {
	raw := `{
      "type": "object",
      "properties": {
        "path":    {"type": "string", "description": "Absolute path to the file"},
        "mode":    {"type": "string", "enum": ["read", "write", "append"]},
        "limit":   {"type": ["integer", "null"], "description": "Max lines"},
        "options": {"$ref": "#/$defs/options"},
        "edits": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "old":     {"type": "string"},
              "new":     {"type": "string"},
              "all":     {"type": "boolean"},
              "nested":  {"$ref": "#/$defs/options"}
            },
            "required": ["old", "new"]
          }
        }
      },
      "required": ["path"],
      "$defs": {
        "options": {
          "type": "object",
          "properties": {
            "encoding":  {"type": "string", "enum": ["utf8", "utf16", "latin1"]},
            "backup":    {"type": "boolean"},
            "retries":   {"type": "integer"},
            "threshold": {"type": "number"}
          }
        }
      }
    }`
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return out
}

func BenchmarkNormalizeToolParametersOneTool(b *testing.B) {
	schema := agentToolSchema()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NormalizeToolParameters(schema)
	}
}

// A realistic agent turn: ~20 tools, all normalised before the request goes out.
func BenchmarkNormalizeToolParametersFullToolset(b *testing.B) {
	tools := make([]any, 20)
	for i := range tools {
		tools[i] = agentToolSchema()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range tools {
			_ = NormalizeToolParameters(t)
		}
	}
}

// Guard: cleaning must not mutate the caller's schema, or the second turn of an
// agent session would send a schema already stripped by the first.
func TestNormalizeDoesNotMutateInput(t *testing.T) {
	schema := agentToolSchema()
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for i := 0; i < 3; i++ {
		_ = NormalizeToolParameters(schema)
	}

	after, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("input schema was mutated by normalisation\nbefore: %s\nafter:  %s",
			before, after)
	}
	fmt.Fprint(io_Discard{}, "")
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }
