package audit

import (
	"fmt"
	"slices"
)

type schemaType byte

const (
	typeAbsent schemaType = iota
	typeBool
	typeUnsigned
	typeSigned
	typeBytes
	typeText
	typeTimestamp
	typeUUID
	typeDigest
	typeList
	typeRecord
)

type valueRule struct {
	typeOf      schemaType
	optional    bool
	textValues  []string
	recordKinds []string
	listElement *valueRule
	listPolicy  collectionPolicy
}

type fieldRule struct {
	name string
	rule valueRule
}

type recordSchema struct {
	fields []fieldRule
}

var (
	absentRule    = valueRule{typeOf: typeAbsent}
	boolRule      = valueRule{typeOf: typeBool}
	unsignedRule  = valueRule{typeOf: typeUnsigned}
	bytesRule     = valueRule{typeOf: typeBytes}
	textRule      = valueRule{typeOf: typeText}
	timestampRule = valueRule{typeOf: typeTimestamp}
	uuidRule      = valueRule{typeOf: typeUUID}
	digestRule    = valueRule{typeOf: typeDigest}

	optionalUnsigned  = optional(unsignedRule)
	optionalBytes     = optional(bytesRule)
	optionalText      = optional(textRule)
	optionalTimestamp = optional(timestampRule)
	optionalUUID      = optional(uuidRule)
	optionalDigest    = optional(digestRule)
)

var recordSchemas = map[string]recordSchema{
	"unknown_origin": schema(
		field("node_id", unsignedRule),
		field("parent_id", absentRule),
		field("name", optionalBytes),
	),
	"known_origin": schema(
		field("node_id", unsignedRule),
		field("parent_id", unsignedRule),
		field("name", bytesRule),
	),
	"topology_node": schema(
		field("node_id", unsignedRule),
		field("parent_id", optionalUnsigned),
		field("name", bytesRule),
		field("node_kind", textEnum("file", "dir")),
		field("state", textEnum("live", "trash", "tombstone")),
		field("origin", optional(recordOf("known_origin", "unknown_origin"))),
		field("created_at", timestampRule),
		field("modified_at", timestampRule),
		field("trashed_at", optionalTimestamp),
	),
	"content_version": schema(
		field("version_id", uuidRule),
		field("node_id", unsignedRule),
		field("blob_hash", digestRule),
		field("size", unsignedRule),
		field("media_type", optionalText),
		field("recorded_at", timestampRule),
		field("node_revision", unsignedRule),
		field("introduced_operation_id", uuidRule),
		field("transition_kind", textEnum("content_create", "content_replace", "content_revert")),
		field("source_version_id", optionalUUID),
	),
	"tag_definition": schema(
		field("tag_id", uuidRule),
		field("name", textRule),
	),
	"tag_assignment": schema(
		field("tag_id", uuidRule),
		field("node_id", unsignedRule),
	),
	"ingest": schema(
		field("ingest_id", uuidRule),
		field("started_at", timestampRule),
		field("source_kind", textRule),
		field("source_desc", bytesRule),
	),
	"provenance_identity": schema(
		field("node_id", unsignedRule),
		field("ingest_id", uuidRule),
		field("original_path", optionalBytes),
		field("original_mtime", optionalTimestamp),
		field("supersedes", optionalDigest),
	),
	"provenance": schema(
		field("identity", digestRule),
		field("node_id", unsignedRule),
		field("ingest_id", uuidRule),
		field("original_path", optionalBytes),
		field("original_mtime", optionalTimestamp),
		field("supersedes", optionalDigest),
	),
	"tag_definition_identity": schema(field("tag_id", uuidRule)),
	"tag_assignment_identity": schema(
		field("tag_id", uuidRule),
		field("node_id", unsignedRule),
	),
	"ingest_identity":         schema(field("ingest_id", uuidRule)),
	"provenance_identity_ref": schema(field("identity", digestRule)),
	"event_identity": schema(
		field("operation_id", uuidRule),
		field("event_ordinal", unsignedRule),
	),
	"member_state": schema(
		field("node_id", unsignedRule),
		field("node_revision", unsignedRule),
		field("current_version_id", optionalUUID),
	),
	"member_state_change": schema(
		field("node_id", unsignedRule),
		field("prior_revision", unsignedRule),
		field("resulting_revision", unsignedRule),
		field("prior_current_version_id", optionalUUID),
		field("resulting_current_version_id", optionalUUID),
	),
	"witnessed_state": schema(field("node", recordOf("topology_node"))),
	"witness": schema(
		field("node_id", unsignedRule),
		field("generation_operation_id", uuidRule),
		field("state_digest", digestRule),
	),
	"baseline_binding": schema(
		field("scope_id", uuidRule),
		field("target_node_id", unsignedRule),
		field("baseline_digest", digestRule),
	),
	"topology_change": schema(
		field("node_id", unsignedRule),
		field("pre", optional(recordOf("topology_node"))),
		field("post", optional(recordOf("topology_node"))),
	),
	"path_state": schema(
		field("path", bytesRule),
		field("state", textEnum("live", "trash", "tombstone")),
	),
	"path_effect": schema(
		field("scope_id", uuidRule),
		field("member_node_id", unsignedRule),
		field("old", recordOf("path_state")),
		field("new", recordOf("path_state")),
	),
	"witness_change": schema(
		field("node_id", unsignedRule),
		field("generation_operation_id", uuidRule),
		field("action", textEnum("create", "retire")),
		field("state_digest", optionalDigest),
	),
	"attached_metadata_change": schema(
		field("record_kind", attachedRecordKindRule()),
		field("stable_identity", attachedIdentityRule()),
		field("pre", optional(attachedRecordRule())),
		field("post", optional(attachedRecordRule())),
	),
	"audit_event": schema(
		field("event_id", digestRule),
		field("operation_id", uuidRule),
		field("node_id", unsignedRule),
		field("event_kind", eventKindRule()),
		field("scope_id", uuidRule),
		field("target_node_id", optionalUnsigned),
		field("attachment_kind", optional(textEnum("provenance", "tag"))),
		field("attachment_identity", optional(eventAttachmentIdentityRule())),
		field("source_version_id", optionalUUID),
		field("event_ordinal", unsignedRule),
		field("recorded_at", timestampRule),
		field("prior_node_revision", unsignedRule),
		field("resulting_node_revision", unsignedRule),
		field("prior_current_version_id", optionalUUID),
		field("resulting_current_version_id", optionalUUID),
		field("origin", originRule()),
		field("agent_label", optionalText),
		field("pre", optional(eventPayloadRule())),
		field("post", optional(eventPayloadRule())),
		field("topology_delta", optionalDigest),
		field("baseline_digest", optionalDigest),
	),
	"enrollment_baseline": schema(
		field("vault_id", uuidRule),
		field("scope_id", uuidRule),
		field("target_node_id", unsignedRule),
		field("operation_id", uuidRule),
		field("cause", textRule),
		field("members", orderedListOf(unsignedRule, collectionUnsigned)),
		field("member_states", orderedListOf(recordOf("member_state"), collectionNodeID)),
		field("nodes", orderedListOf(recordOf("topology_node"), collectionNodeID)),
		field("versions", orderedListOf(recordOf("content_version"), collectionVersion)),
		field("attachments", orderedListOf(attachedRecordRule(), collectionAttachedRecord)),
		field("witnesses", orderedListOf(recordOf("witness"), collectionWitness)),
	),
	"topology_genesis": schema(
		field("vault_id", uuidRule),
		field("lineage_id", uuidRule),
		field("nodes", orderedListOf(recordOf("topology_node"), collectionNodeID)),
	),
	"attached_metadata_genesis": schema(
		field("vault_id", uuidRule),
		field("lineage_id", uuidRule),
		field("records", orderedListOf(attachedRecordRule(), collectionAttachedRecord)),
	),
	"topology_delta": schema(
		field("operation_id", uuidRule),
		field("changes", orderedListOf(recordOf("topology_change"), collectionTopologyChange)),
	),
	"path_effect_list": schema(
		field("operation_id", uuidRule),
		field("topology_delta", digestRule),
		field("effects", orderedListOf(recordOf("path_effect"), collectionPathEffect)),
	),
	"witness_change_list": schema(
		field("operation_id", uuidRule),
		field("changes", orderedListOf(recordOf("witness_change"), collectionWitnessChange)),
	),
	"attached_metadata_delta": schema(
		field("operation_id", uuidRule),
		field("changes", orderedListOf(recordOf("attached_metadata_change"), collectionAttachedChange)),
	),
	"event": schema(field("event", recordOf("audit_event"))),
	"canonical_mutation": schema(
		field("vault_id", uuidRule),
		field("operation_sequence", unsignedRule),
		field("operation_id", uuidRule),
		field("grouping_id", optionalUUID),
		field("recorded_at", timestampRule),
		field("origin", originRule()),
		field("agent_label", optionalText),
		field("events", orderedListOf(recordOf("audit_event"), collectionEvent)),
		field("member_state_changes", orderedListOf(recordOf("member_state_change"), collectionNodeID)),
		field("baselines", orderedListOf(recordOf("baseline_binding"), collectionBaselineBinding)),
		field("topology_delta", optionalDigest),
		field("path_effect_count", unsignedRule),
		field("path_effect_digest", optionalDigest),
		field("witness_change_count", unsignedRule),
		field("witness_change_digest", optionalDigest),
		field("attached_metadata_change_count", unsignedRule),
		field("attached_metadata_change_digest", optionalDigest),
	),
	"scope_chain_entry": schema(
		field("vault_id", uuidRule),
		field("scope_id", uuidRule),
		field("entry_count", unsignedRule),
		field("previous_head", optionalDigest),
		field("mutation_hash", digestRule),
	),
	"allocation_genesis": schema(
		field("vault_id", uuidRule),
		field("lineage_id", uuidRule),
		field("previous_head", optionalDigest),
		field("node_id_high_water", unsignedRule),
		field("operation_sequence_high_water", unsignedRule),
		field("topology_count", unsignedRule),
		field("topology_digest", digestRule),
		field("attached_metadata_count", unsignedRule),
		field("attached_metadata_digest", digestRule),
	),
	"allocation_entry": schema(
		field("vault_id", uuidRule),
		field("lineage_id", uuidRule),
		field("previous_head", digestRule),
		field("operation_sequence", unsignedRule),
		field("operation_id", uuidRule),
		field("allocated_node_ids", orderedListOf(unsignedRule, collectionAllocatedNodeID)),
		field("node_id_high_water", unsignedRule),
		field("operation_sequence_high_water", unsignedRule),
		field("has_audited_mutation", boolRule),
		field("mutation_hash", optionalDigest),
		field("has_topology_change", boolRule),
		field("topology_delta", optionalDigest),
		field("has_witness_change", boolRule),
		field("witness_change_count", unsignedRule),
		field("witness_change_digest", optionalDigest),
		field("has_attached_metadata_change", boolRule),
		field("attached_metadata_change_count", unsignedRule),
		field("attached_metadata_change_digest", optionalDigest),
	),
	"preview_token": schema(
		field("secret", bytesRule),
		field("vault_id", uuidRule),
		field("scope_id", uuidRule),
		field("target_node_id", unsignedRule),
		field("baseline_digest", digestRule),
		field("preview_generation", unsignedRule),
		field("operation_id", uuidRule),
		field("lineage_id", uuidRule),
		field("topology_genesis_digest", optionalDigest),
		field("attached_metadata_genesis_digest", optionalDigest),
	),
}

// Validate checks that a record belongs to the metadata-v1 registry and has
// its exact recursively registered shape.
func Validate(record Record) error {
	return validateRecord(record, 0)
}

func validateRecord(record Record, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("audit record nesting exceeds %d levels", maxValueDepth)
	}
	schema, ok := recordSchemas[record.Kind]
	if !ok {
		return fmt.Errorf("unknown metadata-v1 audit record kind %q", record.Kind)
	}
	provided := make(map[string]Value, len(record.Fields))
	for _, actual := range record.Fields {
		if !validToken(actual.Name) {
			return fmt.Errorf("invalid field name %q in audit record %s", actual.Name, record.Kind)
		}
		if _, exists := provided[actual.Name]; exists {
			return fmt.Errorf("duplicate field %q in audit record %s", actual.Name, record.Kind)
		}
		provided[actual.Name] = actual.Value
	}
	for _, expected := range schema.fields {
		actual, exists := provided[expected.name]
		if !exists {
			return fmt.Errorf("missing field %s.%s", record.Kind, expected.name)
		}
		if err := validateValue(actual, expected.rule, record.Kind+"."+expected.name, depth); err != nil {
			return err
		}
		delete(provided, expected.name)
	}
	if len(provided) != 0 {
		extra := make([]string, 0, len(provided))
		for name := range provided {
			extra = append(extra, name)
		}
		slices.Sort(extra)
		return fmt.Errorf("unexpected field %s.%s", record.Kind, extra[0])
	}
	return nil
}

func validateValue(value Value, rule valueRule, path string, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("audit record nesting exceeds %d levels", maxValueDepth)
	}
	if value.kind == kindAbsent {
		if rule.optional || rule.typeOf == typeAbsent {
			return nil
		}
		return fmt.Errorf("field %s must be %s, not absent", path, rule.describe())
	}
	if rule.typeOf == typeAbsent {
		return fmt.Errorf("field %s must be absent", path)
	}
	if !rule.acceptsKind(value.kind) {
		return fmt.Errorf("field %s must be %s", path, rule.describe())
	}
	if rule.typeOf == typeText && len(rule.textValues) != 0 &&
		!slices.Contains(rule.textValues, string(value.data)) {
		return fmt.Errorf("field %s contains unknown metadata-v1 code %q", path, value.data)
	}
	switch rule.typeOf {
	case typeAbsent, typeBool, typeUnsigned, typeSigned, typeBytes, typeText, typeTimestamp, typeUUID, typeDigest:
	case typeList:
		for index, item := range value.items {
			if err := validateValue(item, *rule.listElement, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
		if err := validateCollection(value.items, rule.listPolicy, path); err != nil {
			return err
		}
	case typeRecord:
		if value.record == nil {
			return fmt.Errorf("field %s contains a nil audit record", path)
		}
		if !slices.Contains(rule.recordKinds, value.record.Kind) {
			return fmt.Errorf("field %s contains disallowed audit record kind %q", path, value.record.Kind)
		}
		if err := validateRecord(*value.record, depth+1); err != nil {
			return fmt.Errorf("field %s: %w", path, err)
		}
	}
	return nil
}

func (rule valueRule) acceptsKind(kind valueKind) bool {
	switch rule.typeOf {
	case typeBool:
		return kind == kindFalse || kind == kindTrue
	case typeUnsigned:
		return kind == kindUnsigned
	case typeSigned:
		return kind == kindSigned
	case typeBytes:
		return kind == kindBytes
	case typeText:
		return kind == kindText
	case typeTimestamp:
		return kind == kindTimestamp
	case typeUUID:
		return kind == kindUUID
	case typeDigest:
		return kind == kindDigest
	case typeList:
		return kind == kindList
	case typeRecord:
		return kind == kindRecord
	default:
		return kind == kindAbsent
	}
}

func (rule valueRule) describe() string {
	names := [...]string{"absent", "boolean", "unsigned integer", "signed integer", "bytes", "text", "timestamp", "UUID", "digest", "list", "nested record"}
	return names[rule.typeOf]
}

func schema(fields ...fieldRule) recordSchema { return recordSchema{fields: fields} }

func field(name string, rule valueRule) fieldRule { return fieldRule{name: name, rule: rule} }

func optional(rule valueRule) valueRule {
	rule.optional = true
	return rule
}

func textEnum(values ...string) valueRule {
	return valueRule{typeOf: typeText, textValues: values}
}

func recordOf(kinds ...string) valueRule {
	return valueRule{typeOf: typeRecord, recordKinds: kinds}
}

func listOf(element valueRule) valueRule {
	return valueRule{typeOf: typeList, listElement: &element}
}

func orderedListOf(element valueRule, policy collectionPolicy) valueRule {
	rule := listOf(element)
	rule.listPolicy = policy
	return rule
}

func attachedRecordRule() valueRule {
	return recordOf("ingest", "provenance", "tag_assignment", "tag_definition")
}

func attachedIdentityRule() valueRule {
	return recordOf("ingest_identity", "provenance_identity_ref", "tag_assignment_identity", "tag_definition_identity")
}

func eventAttachmentIdentityRule() valueRule {
	return recordOf("provenance_identity_ref", "tag_assignment_identity", "tag_definition_identity")
}

func attachedRecordKindRule() valueRule {
	return textEnum("ingest", "provenance", "tag_assignment", "tag_definition")
}

func eventPayloadRule() valueRule {
	return recordOf("content_version", "path_state", "provenance", "tag_assignment", "tag_definition", "topology_node")
}

func eventKindRule() valueRule {
	return textEnum(
		"audit_enroll", "audit_inherit", "content_create", "content_replace", "content_revert",
		"node_create", "node_path", "provenance_add", "provenance_supersede", "tag_assign",
		"tag_define", "tag_delete", "tag_rename", "tag_unassign",
	)
}

func originRule() valueRule { return textEnum("api", "cli", "import", "job") }
