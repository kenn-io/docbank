//nolint:goconst // Keep report wire field names beside their closed schemas.
package mcp

import "math"

func reportIDSchema() schema {
	return schema{"type": "string", "pattern": "^[0-9a-f]{48}$", "minLength": 48, "maxLength": 48}
}

func reportIdentitySchema() schema {
	return objectSchema(schema{"node_id": integerSchema(1, math.MaxInt64),
		"version_id": uuidSchema(), "sha256": sha256Schema()}, "node_id", "version_id", "sha256")
}

func reportTermSchema() schema {
	date := schema{"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}
	return objectSchema(schema{"number": integerSchema(1, math.MaxInt64),
		"expression": stringSchema(8192), "syntax": enumSchema("simple", "advanced"),
		"dates": objectSchema(schema{"start": date, "end": date}, "start", "end")},
		"number", "expression", "syntax", "dates")
}

func reportChoiceSchema() schema {
	return objectSchema(schema{
		"document":        reportIdentitySchema(),
		"candidate_id":    sha256Schema(),
		"evidence_sha256": sha256Schema(),
		"reason":          stringSchema(4096), "action": enumSchema("select", "interpret", "reclassify"),
		"reviewed_date": stringSchema(10), "reviewed_timezone": stringSchema(128),
		"reviewed_role": enumSchema("document_date",
			"created",
			"authored",
			"sent",
			"captured",
			"signed",
			"effective",
			"expiry"),
	}, "document", "candidate_id", "evidence_sha256", "reason", "action")
}

func reportReceiptSchema() schema {
	result := rootObjectSchema(withPrivateCache(schema{
		"report_id": reportIDSchema(), "parent_id": reportIDSchema(),
		"state": enumSchema("complete", "needs_review"), "observed_at": dateTimeSchema(),
		"expires_at": dateTimeSchema(), "unresolved_dates": integerSchema(0, 50000),
		"bundle_bytes": integerSchema(1, 512<<20), "bundle_sha256": sha256Schema(),
	}), cacheRequired("report_id", "state", "observed_at", "expires_at", "unresolved_dates")...)
	result["oneOf"] = []schema{
		{"properties": schema{"state": schema{"const": "complete"}},
			"required": []string{"bundle_bytes",
				"bundle_sha256"}},
		{"properties": schema{"state": schema{"const": "needs_review"}}, "not": schema{"anyOf": []schema{
			{"required": []string{"bundle_bytes"}}, {"required": []string{"bundle_sha256"}},
		}}},
	}
	return result
}

func createReportSchemas() (schema, schema) {
	documents := arraySchema(reportIdentitySchema(), 1000)
	documents["minItems"] = 1
	terms := arraySchema(reportTermSchema(), 128)
	terms["minItems"] = 1
	request := objectSchema(schema{
		"version": integerSchema(1, 1), "all_documents": schema{"type": "boolean", "const": false},
		"selected_documents": objectSchema(schema{"documents": documents}, "documents"),
		"timezone":           stringSchema(128),
		"source_timezone":    stringSchema(128),
		"profile":            stringSchema(128),
		"numeric_date_order": enumSchema("MDY",
			"DMY"),
		"coverage_mode": enumSchema("strict",
			"available_only"),
		"terms": terms,
	}, "version", "selected_documents", "timezone", "terms")
	return rootObjectSchema(schema{"request": request}, "request"), reportReceiptSchema()
}

func reviseReportSchemas() (schema, schema) {
	choices := arraySchema(reportChoiceSchema(), 1000)
	choices["minItems"] = 1
	return rootObjectSchema(schema{"report_id": reportIDSchema(), "choices": choices},
		"report_id", "choices"), reportReceiptSchema()
}

func getReportSummarySchemas() (schema, schema) {
	counts := objectSchema(schema{
		"hits": integerSchema(0, 0), "hits_plus_family": integerSchema(0, 0),
		"unique_hits": integerSchema(0, 0), "unique_families": integerSchema(0, 0),
		"unique_hits_plus_family": integerSchema(0, 0),
	}, "hits", "hits_plus_family", "unique_hits", "unique_families", "unique_hits_plus_family")
	coverage := objectSchema(schema{
		"scoped":              integerSchema(0, 0),
		"searchable":          integerSchema(0, 0),
		"missing_text":        integerSchema(0, 0),
		"incomplete_families": integerSchema(0, 0), "fallback_dates": integerSchema(0, 0),
		"warnings": arraySchema(stringSchema(0), 0),
	}, "scoped", "searchable", "missing_text", "incomplete_families", "fallback_dates")
	summary := objectSchema(schema{
		"id":        reportIDSchema(),
		"parent_id": reportIDSchema(),
		"state": enumSchema("complete",
			"needs_review"),
		"observed_at":      dateTimeSchema(),
		"expires_at":       dateTimeSchema(),
		"terms":            arraySchema(reportTermSchema(), 128),
		"counts":           arraySchema(counts, 128),
		"coverage":         coverage,
		"row_coverage":     arraySchema(coverage, 128),
		"unresolved_dates": integerSchema(0, 50000),
		"csv_sha256":       sha256Schema(),
		"bundle_sha256":    sha256Schema(),
		"csv_bytes":        integerSchema(0, 512<<20), "bundle_bytes": integerSchema(0, 512<<20),
	}, "id", "state", "observed_at", "expires_at", "terms", "coverage", "unresolved_dates")
	return rootObjectSchema(schema{"report_id": reportIDSchema()}, "report_id"),
		rootObjectSchema(withPrivateCache(schema{"summary": summary}), cacheRequired("summary")...)
}

func reportCandidateSchema() schema {
	locator := objectSchema(schema{
		"evidence_id":     stringSchema(0),
		"evidence_sha256": stringSchema(0),
		"rendition_id":    stringSchema(0),
		"text_sha256":     stringSchema(0), "page": integerSchema(0, 0), "start_byte": integerSchema(0, 0),
		"end_byte": integerSchema(0, 0), "quote": stringSchema(0),
	}, "start_byte", "end_byte")
	return objectSchema(schema{
		"id": sha256Schema(), "document": reportIdentitySchema(), "role": stringSchema(0),
		"source_class": stringSchema(0), "raw": stringSchema(0), "value": stringSchema(0),
		"precision": stringSchema(0), "timezone": stringSchema(0), "confidence": stringSchema(0),
		"source_namespace": stringSchema(0),
		"source_field":     stringSchema(0),
		"claim_basis":      stringSchema(0),
		"locator":          locator, "rejection": stringSchema(0),
	}, "id", "document", "role", "source_class", "raw", "locator")
}

func getReportDatesSchemas() (schema, schema) {
	selection := objectSchema(schema{
		"candidate_id": stringSchema(64), "date": stringSchema(0), "rule_id": stringSchema(0),
		"reason": stringSchema(0), "mode": stringSchema(0),
	}, "candidate_id", "date", "rule_id", "reason", "mode")
	member := objectSchema(schema{
		"candidates_complete": booleanSchema(), "document": reportIdentitySchema(),
		"candidates": arraySchema(reportCandidateSchema(), 256),
		"selection":  selection,
		"choice":     reportChoiceSchema(),
	}, "candidates_complete", "document", "candidates", "selection")
	page := objectSchema(schema{"members": arraySchema(member, 100),
		"next_cursor": stringSchema(4096)},
		"members")
	return rootObjectSchema(schema{"report_id": reportIDSchema(), "cursor": stringSchema(4096),
			"limit": integerSchema(1, 100)}, "report_id"), rootObjectSchema(withPrivateCache(schema{
			"report_id": reportIDSchema(), "page": page,
		}), cacheRequired("report_id", "page")...)
}
