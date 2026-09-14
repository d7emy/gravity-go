package antigravity

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSON Schema cleaning for Gemini's v1internal API.
//
// Gemini accepts a narrow subset of JSON Schema and rejects the whole request
// when a tool declaration steps outside it. This pass:
//  1. expands $ref/$defs inline,
//  2. hard-removes unsupported keywords,
//  3. folds validation keywords into the description so the model still sees them,
//  4. collapses union types (["string","null"] -> "string") and anyOf/oneOf,
//  5. lowercases type names and stringifies enum values,
//  6. drops required entries with no matching property.

// validationFields are migrated into the description rather than dropped, so the
// model still learns the constraint even though Gemini will not enforce it.
var validationFields = []string{
	"pattern", "minLength", "maxLength", "minimum", "maximum",
	"minItems", "maxItems", "exclusiveMinimum", "exclusiveMaximum",
	"multipleOf",
}

// hardRemoveFields are stripped outright: Gemini rejects them.
var hardRemoveFields = []string{
	// $ref only survives to this point when it could not be resolved.
	"$schema", "$id", "$ref", "additionalProperties", "enumCaseInsensitive",
	"enumNormalizeWhitespace", "uniqueItems", "default", "const",
	"examples", "propertyNames", "anyOf", "oneOf", "allOf", "not",
	"if", "then", "else", "dependencies", "dependentSchemas",
	"dependentRequired", "cache_control", "contentEncoding",
	"contentMediaType", "deprecated", "readOnly", "writeOnly",
	"format",
}

// maxRefDepth stops a self-referencing definition (a tree node whose children
// are the same type) from expanding forever.
const maxRefDepth = 8

// CleanJSONSchemaForGemini returns a Gemini-safe copy of schema.
func CleanJSONSchemaForGemini(schema any) any {
	node, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	// Deep copy so the caller's schema is never mutated.
	value, ok := deepCopy(node).(map[string]any)
	if !ok {
		return schema
	}

	// Step 0: hoist definitions, then inline every $ref against them.
	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if raw, ok := value[key].(map[string]any); ok {
			for k, v := range raw {
				defs[k] = v
			}
			delete(value, key)
		}
	}
	flattenRefs(value, defs, 0)

	// Step 1: recursive clean.
	cleanRecursive(value)
	return value
}

func deepCopy(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, item := range node {
			out[k] = deepCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = deepCopy(item)
		}
		return out
	}
	return v
}

func flattenRefs(obj map[string]any, defs map[string]any, depth int) {
	if ref, ok := obj["$ref"].(string); ok {
		refName := ref
		if idx := strings.LastIndex(ref, "/"); idx >= 0 {
			refName = ref[idx+1:]
		}
		delete(obj, "$ref")

		target, found := defs[refName].(map[string]any)
		switch {
		case found:
			if depth >= maxRefDepth {
				// Stop recursing and leave a permissive node behind.
				if _, hasType := obj["type"]; !hasType {
					obj["type"] = "object"
				}
				return
			}
			for k, v := range target {
				if _, exists := obj[k]; !exists {
					obj[k] = deepCopy(v)
				}
			}
			flattenRefs(obj, defs, depth+1)
		case len(obj) == 0:
			// Unresolvable ref (external document, or a nested pointer whose last
			// segment is not a definition name). An empty node tells the model
			// nothing; a permissive object at least keeps the parameter usable.
			obj["type"] = "object"
		}
	}

	for _, v := range obj {
		switch child := v.(type) {
		case map[string]any:
			flattenRefs(child, defs, depth)
		case []any:
			for _, item := range child {
				if node, ok := item.(map[string]any); ok {
					flattenRefs(node, defs, depth)
				}
			}
		}
	}
}

func cleanRecursive(v any) {
	switch node := v.(type) {
	case []any:
		for _, item := range node {
			cleanRecursive(item)
		}
		return
	case map[string]any:
		// 1. Children first.
		for _, child := range node {
			cleanRecursive(child)
		}

		// 2. Fold validation keywords into the description.
		var constraints []string
		for _, field := range validationFields {
			raw, present := node[field]
			if !present {
				continue
			}
			// Only migrate scalars. An object here is a property actually named
			// e.g. "pattern", which must be preserved.
			switch scalar := raw.(type) {
			case string:
				constraints = append(constraints, fmt.Sprintf("%s: %s", field, scalar))
				delete(node, field)
			case bool:
				constraints = append(constraints, fmt.Sprintf("%s: %t", field, scalar))
				delete(node, field)
			case float64:
				constraints = append(constraints, fmt.Sprintf("%s: %s", field, formatNumber(scalar)))
				delete(node, field)
			case json.Number:
				constraints = append(constraints, fmt.Sprintf("%s: %s", field, scalar.String()))
				delete(node, field)
			}
		}
		if len(constraints) > 0 {
			existing, _ := node["description"].(string)
			node["description"] = existing + " [Constraint: " + strings.Join(constraints, ", ") + "]"
		}

		// 3. Salvage a type from anyOf/oneOf before they are removed.
		if _, hasType := node["type"]; !hasType {
			union, ok := node["anyOf"].([]any)
			if !ok {
				union, _ = node["oneOf"].([]any)
			}
			if extracted := extractTypeFromUnion(union); extracted != "" {
				node["type"] = extracted
			}
		}

		// 4. Remove the blacklist.
		for _, field := range hardRemoveFields {
			delete(node, field)
		}

		// 5. required must only name existing properties.
		if required, ok := node["required"].([]any); ok {
			properties, hasProps := node["properties"].(map[string]any)
			filtered := make([]any, 0, len(required))
			if hasProps {
				for _, item := range required {
					if name, ok := item.(string); ok {
						if _, exists := properties[name]; exists {
							filtered = append(filtered, name)
						}
					}
				}
			}
			node["required"] = filtered
		}

		// 6. Normalize type: ["string","null"] -> "string", lowercase.
		switch typeValue := node["type"].(type) {
		case []any:
			selected := "string"
			for _, item := range typeValue {
				if s, ok := item.(string); ok && s != "null" {
					selected = strings.ToLower(s)
					break
				}
			}
			node["type"] = selected
		case string:
			node["type"] = strings.ToLower(typeValue)
		}

		// 7. Gemini requires enum values to be strings.
		if enum, ok := node["enum"].([]any); ok {
			converted := make([]any, 0, len(enum))
			for _, item := range enum {
				converted = append(converted, stringifyEnumValue(item))
			}
			node["enum"] = converted
		}
	}
}

func extractTypeFromUnion(union []any) string {
	for _, item := range union {
		node, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typeStr, ok := node["type"].(string); ok && typeStr != "null" {
			return strings.ToLower(typeStr)
		}
	}
	return ""
}

func stringifyEnumValue(item any) string {
	switch v := item.(type) {
	case string:
		return v
	case float64:
		return formatNumber(v)
	case json.Number:
		return v.String()
	case bool:
		return fmt.Sprintf("%t", v)
	case nil:
		return "null"
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// formatNumber renders a float the way JSON.stringify would: no trailing ".0"
// on integral values.
func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", v), "0"), ".")
}

// NormalizeToolParameters coerces whatever the client sent into an object schema
// Gemini will accept.
func NormalizeToolParameters(schema any) map[string]any {
	empty := func() map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if schema == nil {
		return empty()
	}

	// Some clients send the schema as a JSON string.
	if asString, ok := schema.(string); ok {
		trimmed := strings.TrimSpace(asString)
		if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
			return empty()
		}
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return empty()
		}
		schema = parsed
	}

	node, ok := schema.(map[string]any)
	if !ok {
		return empty()
	}

	cleaned, ok := CleanJSONSchemaForGemini(node).(map[string]any)
	if !ok {
		return empty()
	}

	if t, _ := cleaned["type"].(string); t != "object" {
		cleaned["type"] = "object"
	}
	if _, ok := cleaned["properties"].(map[string]any); !ok {
		cleaned["properties"] = map[string]any{}
	}
	if _, ok := cleaned["required"].([]any); !ok {
		delete(cleaned, "required")
	}
	return cleaned
}
