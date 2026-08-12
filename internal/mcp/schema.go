package mcp

const jsonSchema202012 = "https://json-schema.org/draft/2020-12/schema"

func queryInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"question": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxQuestionRunes,
				"description": "Natural-language question about this corpus. Ask a complete multi-topic question; retrieval may split it into bounded sub-queries.",
			},
			"top_k": map[string]any{
				"type": "integer", "minimum": 1, "maximum": maxTopK, "default": defaultTopK,
				"description": "Maximum number of ranked evidence packets to index.",
			},
		},
		"required": []string{"question"},
	}
}

func cursorInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"cursor": map[string]any{
				"type": "string", "minLength": 1, "maxLength": maxCursorBytes,
				"description": "Opaque next_cursor returned by this MCP session. Pass it unchanged.",
			},
		},
		"required": []string{"cursor"},
	}
}

func evidencePageOutputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind":              map[string]any{"enum": []string{"search", "index", "text"}},
			"result_id":         map[string]any{"type": "string", "pattern": "^R[0-9a-f]{12}$"},
			"question":          nullableStringSchema(),
			"sub_queries":       stringArraySchema(),
			"packets":           map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/packet"}},
			"segments":          map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/segment"}},
			"warnings":          stringArraySchema(),
			"delivery_warnings": stringArraySchema(),
			"evidence_budget":   map[string]any{"$ref": "#/$defs/budget"},
			"response_budget":   map[string]any{"$ref": "#/$defs/budget"},
			"complete":          map[string]any{"type": "boolean"},
			"next_cursor":       nullableStringSchema(),
			"continuation_tool": nullableStringSchema(),
		},
		"required": []string{
			"kind", "result_id", "question", "sub_queries", "packets", "segments", "warnings",
			"delivery_warnings", "evidence_budget", "response_budget", "complete", "next_cursor", "continuation_tool",
		},
		"$defs": map[string]any{
			"budget": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"used": map[string]any{"type": "integer", "minimum": 0}, "allowed": map[string]any{"type": "integer", "minimum": 0}, "unit": map[string]any{"const": utf8ByteUnit},
				},
				"required": []string{"used", "allowed", "unit"},
			},
			"document": documentSchema(),
			"match": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"chunk_id": map[string]any{"type": "string", "minLength": 1}, "start": map[string]any{"type": "integer", "minimum": 0},
					"end": map[string]any{"type": "integer", "minimum": 0}, "offset_unit": map[string]any{"const": utf8ByteUnit},
					"score": map[string]any{"type": "number"}, "legs": map[string]any{"enum": []string{"dense", "lexical", "both"}},
					"sub_query": nullableStringSchema(), "snippet": map[string]any{"type": "string"},
				},
				"required": []string{"chunk_id", "start", "end", "offset_unit", "score", "legs", "sub_query", "snippet"},
			},
			"packet": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"reference": map[string]any{"type": "string", "pattern": "^R[0-9a-f]{12}:E[1-9][0-9]*$"},
					"rank":      map[string]any{"type": "integer", "minimum": 1}, "document": map[string]any{"$ref": "#/$defs/document"},
					"match": map[string]any{"$ref": "#/$defs/match"}, "text_available": map[string]any{"type": "boolean"},
					"text_omitted": map[string]any{"type": "boolean"}, "related_count": map[string]any{"type": "integer", "minimum": 0},
					"related_text_available": map[string]any{"type": "integer", "minimum": 0},
				},
				"required": []string{"reference", "rank", "document", "match", "text_available", "text_omitted", "related_count", "related_text_available"},
			},
			"range": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"start": map[string]any{"type": "integer", "minimum": 0}, "end": map[string]any{"type": "integer", "minimum": 1}, "unit": map[string]any{"const": utf8ByteUnit}},
				"required":   []string{"start", "end", "unit"},
			},
			"segment": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"reference": map[string]any{"type": "string", "pattern": "^R[0-9a-f]{12}:E[1-9][0-9]*$"},
					"relation":  map[string]any{"oneOf": []any{map[string]any{"enum": []string{"parent", "attachment", "same-thread"}}, map[string]any{"type": "null"}}},
					"document":  map[string]any{"$ref": "#/$defs/document"}, "text": map[string]any{"type": "string", "minLength": 1},
					"text_range": map[string]any{"$ref": "#/$defs/range"}, "text_complete": map[string]any{"type": "boolean"},
				},
				"required": []string{"reference", "relation", "document", "text", "text_range", "text_complete"},
			},
		},
	}
}

func documentSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"document_id": map[string]any{"type": "string", "minLength": 1}, "parent_document_id": nullableStringSchema(),
			"thread_id": nullableStringSchema(), "document_type": map[string]any{"type": "string", "minLength": 1},
			"title": map[string]any{"type": "string"}, "from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"},
			"date":          map[string]any{"oneOf": []any{map[string]any{"type": "string", "format": "date-time"}, map[string]any{"type": "null"}}},
			"source_sha256": map[string]any{"type": "string", "minLength": 1}, "tier1_uri": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"document_id", "parent_document_id", "thread_id", "document_type", "title", "from", "to", "date", "source_sha256", "tier1_uri"},
	}
}

func nullableStringSchema() map[string]any {
	return map[string]any{"type": []string{"string", "null"}}
}

func stringArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}
