package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type connectionSuggestionOutput struct {
	api.ConnectionSuggestionReport
	privateCache
}

func suggestConnections(ctx context.Context, lease *daemonLease, raw []byte) (connectionSuggestionOutput, error) {
	var request api.ConnectionSuggestionRequest
	if err := decodeReadArguments(raw, &request); err != nil {
		return connectionSuggestionOutput{}, err
	}
	report, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (api.ConnectionSuggestionReport, error) {
		return client.SuggestConnections(ctx, request)
	})
	if err != nil {
		return connectionSuggestionOutput{}, err
	}
	return connectionSuggestionOutput{ConnectionSuggestionReport: report, privateCache: newPrivateCache()}, nil
}

func connectionPassageSchema() schema {
	return objectSchema(schema{
		"version":               integerSchema(1, 1),
		"federation_domain_uid": uuidSchema(),
		"vault_uid":             uuidSchema(),
		"document_uid":          uuidSchema(),
		"content_version_id":    uuidSchema(), //nolint:goconst // Stable wire field is shared by tool schemas.
		"source_sha256":         sha256Schema(),
		"rendition_build_id":    sha256Schema(),
		"attachment_id":         sha256Schema(),
		"body_sha256":           sha256Schema(),
		"byte_start":            integerSchema(0, 0),
		"byte_end":              integerSchema(1, 0),
		"quote_sha256":          sha256Schema(),
	}, "version", "vault_uid", "document_uid", "content_version_id", "source_sha256",
		"rendition_build_id", "attachment_id", "body_sha256", "byte_start", "byte_end", "quote_sha256")
}

func connectionSpanSchema() schema {
	return objectSchema(schema{
		"UnitIndex": integerSchema(0, 0), "CharStart": integerSchema(0, 0), "CharEnd": integerSchema(0, 0),
	}, "UnitIndex", "CharStart", "CharEnd")
}

func suggestConnectionsSchemas() (schema, schema) {
	passage := connectionPassageSchema()
	fence := objectSchema(schema{
		"vault_uid": uuidSchema(),
		"content_version_ids": schema{"type": "array", "items": uuidSchema(), "minItems": 1, //nolint:goconst // JSON Schema vocabulary is repeated.
			"maxItems": 4096, "uniqueItems": true},
	}, "vault_uid", "content_version_ids")
	input := rootObjectSchema(schema{
		"source":     passage,
		"seed_kind":  enumSchema("passage", "document"),
		"profile":    schema{"type": "string", "minLength": 1, "maxLength": 128}, //nolint:goconst // JSON Schema vocabulary is repeated.
		"binding_id": schema{"type": "string", "minLength": 1, "maxLength": 128},
		"method":     enumSchema("semantic", "lexical", "tag", "hybrid"),
		"limit":      integerSchema(1, 100),
		"fence":      fence,
	}, "source", "profile", "binding_id", "fence")
	seedSegment := objectSchema(schema{
		"input_id": stringSchema(128), "passage": passage, "quote": stringSchema(0),
		"span": connectionSpanSchema(), "generation_id": stringSchema(128),
	}, "input_id", "passage", "quote", "span", "generation_id")
	duplicateMember := objectSchema(schema{
		"id": sha256Schema(), "target": passage, "target_quote": stringSchema(0),
		"target_node_id": integerSchema(1, 0), "target_path": stringSchema(maxPathCharacters),
		"score": schema{"type": "number"}, "score_metric": stringSchema(128),
		"source_segment": passage, "source_segment_quote": stringSchema(0),
		"source_input_id": stringSchema(128), "source_span": connectionSpanSchema(),
		"source_generation_id": stringSchema(128), "vector_space_id": stringSchema(128),
		"embedding_set_id": stringSchema(128), "input_generation_id": stringSchema(128),
		"input_id": stringSchema(128), "target_span": connectionSpanSchema(),
		"index_generation_id": stringSchema(128),
	}, "id", "target", "target_quote", "target_node_id", "target_path", "score", "score_metric")
	candidate := objectSchema(schema{
		"id": sha256Schema(), "method": enumSchema("semantic", "lexical"),
		"reason": stringSchema(128), "source": passage, "source_quote": stringSchema(0),
		"source_segment": passage, "source_segment_quote": stringSchema(0),
		"source_input_id": stringSchema(128), "source_span": connectionSpanSchema(),
		"source_generation_id": stringSchema(128),
		"target":               passage, "target_quote": stringSchema(0),
		"target_node_id": integerSchema(1, 0), "target_path": stringSchema(maxPathCharacters),
		"score": schema{"type": "number"}, "score_metric": stringSchema(128),
		"aggregation": stringSchema(128), "vector_space_id": stringSchema(128),
		"embedding_set_id": stringSchema(128), "input_generation_id": stringSchema(128),
		"input_id": stringSchema(128), "target_span": connectionSpanSchema(),
		"index_generation_id": stringSchema(128), "duplicate_count": integerSchema(0, 4096),
		"duplicate_members": arraySchema(duplicateMember, 4095),
	}, "id", "method", "reason", "source", "source_quote", "target", "target_quote",
		"target_node_id", "target_path", "score", "score_metric", "aggregation", "duplicate_count", "duplicate_members")
	output := rootObjectSchema(withPrivateCache(schema{
		"state": enumSchema("ready", "unavailable"), "coverage_reason": stringSchema(128),
		"source": passage, "seed_kind": enumSchema("passage", "document"),
		"method":          enumSchema("semantic", "lexical", "tag", "hybrid"),
		"candidate_count": integerSchema(0, 100), "seed_segment_count": integerSchema(0, 0),
		"seed_segments": arraySchema(seedSegment, 4096),
		"aggregation":   stringSchema(128), "score_metric": stringSchema(128),
		"vector_space_id": stringSchema(128), "index_generation_id": stringSchema(128),
		"source_manifest_checksum": stringSchema(128), "source_embedding_set_id": stringSchema(128),
		"truncated": schema{"type": "boolean"}, "candidates": arraySchema(candidate, 100),
		"fallback_methods": arraySchema(enumSchema("lexical", "tag"), 2),
	}), cacheRequired("state", "source", "seed_kind", "method", "candidate_count",
		"seed_segment_count", "seed_segments", "truncated", "candidates", "fallback_methods")...)
	return input, output
}
