package audit

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditRegistryAssignsEveryCollectionPolicy(t *testing.T) {
	expected := map[string]collectionPolicy{
		"allocation_entry.allocated_node_ids":     collectionAllocatedNodeID,
		"attached_metadata_delta.changes":         collectionAttachedChange,
		"attached_metadata_genesis.records":       collectionAttachedRecord,
		"canonical_mutation.baselines":            collectionBaselineBinding,
		"canonical_mutation.events":               collectionEvent,
		"canonical_mutation.member_state_changes": collectionNodeID,
		"enrollment_baseline.attachments":         collectionAttachedRecord,
		"enrollment_baseline.member_states":       collectionNodeID,
		"enrollment_baseline.members":             collectionUnsigned,
		"enrollment_baseline.nodes":               collectionNodeID,
		"enrollment_baseline.versions":            collectionVersion,
		"enrollment_baseline.witnesses":           collectionWitness,
		"path_effect_list.effects":                collectionPathEffect,
		"topology_delta.changes":                  collectionTopologyChange,
		"topology_genesis.nodes":                  collectionNodeID,
		"witness_change_list.changes":             collectionWitnessChange,
	}
	actual := make(map[string]collectionPolicy)
	for kind, registered := range recordSchemas {
		for _, registeredField := range registered.fields {
			if registeredField.rule.typeOf != typeList {
				continue
			}
			actual[kind+"."+registeredField.name] = registeredField.rule.listPolicy
		}
	}
	assert.Equal(t, expected, actual)
	for path, policy := range actual {
		assert.NotEqual(t, collectionNone, policy, path)
	}
}

func TestAuditCollectionPoliciesRejectReversalAndDuplicates(t *testing.T) {
	idA := mustUUID(t, "00112233-4455-4677-8899-aabbccddee00")
	idB := mustUUID(t, "00112233-4455-4677-8899-aabbccddee01")
	digestA := Digest([sha256.Size]byte{})
	digestBytes := [sha256.Size]byte{}
	digestBytes[sha256.Size-1] = 1
	digestB := Digest(digestBytes)

	tagA := replaceField(exampleRecord(t, "tag_definition"), "tag_id", idA)
	tagB := replaceField(exampleRecord(t, "tag_definition"), "tag_id", idB)
	identityA := replaceField(exampleRecord(t, "tag_definition_identity"), "tag_id", idA)
	identityB := replaceField(exampleRecord(t, "tag_definition_identity"), "tag_id", idB)

	tests := []struct {
		name   string
		policy collectionPolicy
		left   Value
		right  Value
	}{
		{name: "unsigned", policy: collectionUnsigned, left: Unsigned(1), right: Unsigned(2)},
		{
			name:   "node ID",
			policy: collectionNodeID,
			left:   nestedWith(t, "member_state", "node_id", Unsigned(1)),
			right:  nestedWith(t, "member_state", "node_id", Unsigned(2)),
		},
		{
			name:   "version UUID",
			policy: collectionVersion,
			left:   nestedWith(t, "content_version", "version_id", idA),
			right:  nestedWith(t, "content_version", "version_id", idB),
		},
		{name: "attached identity", policy: collectionAttachedRecord, left: Nested(tagA), right: Nested(tagB)},
		{
			name:   "witness generation",
			policy: collectionWitness,
			left:   nestedWith(t, "witness", "generation_operation_id", idA),
			right:  nestedWith(t, "witness", "generation_operation_id", idB),
		},
		{
			name:   "topology node",
			policy: collectionTopologyChange,
			left:   nestedWith(t, "topology_change", "node_id", Unsigned(1)),
			right:  nestedWith(t, "topology_change", "node_id", Unsigned(2)),
		},
		{
			name:   "path effect scope",
			policy: collectionPathEffect,
			left:   nestedWith(t, "path_effect", "scope_id", idA),
			right:  nestedWith(t, "path_effect", "scope_id", idB),
		},
		{
			name:   "witness action",
			policy: collectionWitnessChange,
			left:   nestedWith(t, "witness_change", "action", mustText(t, "create")),
			right:  nestedWith(t, "witness_change", "action", mustText(t, "retire")),
		},
		{
			name:   "attached change identity",
			policy: collectionAttachedChange,
			left: nestedWith(t, "attached_metadata_change", "stable_identity",
				Nested(identityA)),
			right: nestedWith(t, "attached_metadata_change", "stable_identity",
				Nested(identityB)),
		},
		{
			name:   "event attachment identity",
			policy: collectionEvent,
			left: nestedWith(t, "audit_event", "attachment_identity",
				Nested(identityA)),
			right: nestedWith(t, "audit_event", "attachment_identity",
				Nested(identityB)),
		},
		{
			name:   "baseline digest",
			policy: collectionBaselineBinding,
			left:   nestedWith(t, "baseline_binding", "baseline_digest", digestA),
			right:  nestedWith(t, "baseline_binding", "baseline_digest", digestB),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, validateCollection([]Value{test.left, test.right}, test.policy, "test.items"))
			require.ErrorContains(t,
				validateCollection([]Value{test.right, test.left}, test.policy, "test.items"),
				"canonical order",
			)
			require.ErrorContains(t,
				validateCollection([]Value{test.left, test.left}, test.policy, "test.items"),
				"duplicate canonical collection key",
			)
		})
	}
}

func TestAuditCollectionPreservesIntrinsicAllocationOrder(t *testing.T) {
	require.NoError(t, validateCollection(
		[]Value{Unsigned(9), Unsigned(2), Unsigned(7)},
		collectionAllocatedNodeID,
		"allocation_entry.allocated_node_ids",
	))
	require.ErrorContains(t, validateCollection(
		[]Value{Unsigned(9), Unsigned(2), Unsigned(9)},
		collectionAllocatedNodeID,
		"allocation_entry.allocated_node_ids",
	), "duplicate canonical collection key")
}

func TestAuditEncodingRejectsNoncanonicalRegisteredCollection(t *testing.T) {
	baseline := replaceField(exampleRecord(t, "enrollment_baseline"), "members",
		List(Unsigned(2), Unsigned(1)))
	require.ErrorContains(t, Validate(baseline), "canonical order")
	_, err := Encode(baseline)
	require.ErrorContains(t, err, "canonical order")
}

func TestAuditEventCollectionSortsAbsentSentinelsFirst(t *testing.T) {
	absentTarget := Nested(exampleRecord(t, "audit_event"))
	presentTarget := nestedWith(t, "audit_event", "target_node_id", Unsigned(1))
	require.NoError(t, validateCollection(
		[]Value{absentTarget, presentTarget}, collectionEvent, "canonical_mutation.events",
	))
	require.ErrorContains(t, validateCollection(
		[]Value{presentTarget, absentTarget}, collectionEvent, "canonical_mutation.events",
	), "canonical order")

	identity := Nested(exampleRecord(t, "tag_definition_identity"))
	absentIdentity := nestedWith(t, "audit_event", "attachment_kind", mustText(t, "tag_assignment"))
	presentIdentity := nestedWithFields(t, "audit_event",
		fieldValue{name: "attachment_kind", value: mustText(t, "tag_assignment")},
		fieldValue{name: "attachment_identity", value: identity},
	)
	require.NoError(t, validateCollection(
		[]Value{absentIdentity, presentIdentity}, collectionEvent, "canonical_mutation.events",
	))
	require.ErrorContains(t, validateCollection(
		[]Value{presentIdentity, absentIdentity}, collectionEvent, "canonical_mutation.events",
	), "canonical order")
}

func TestAuditCollectionsRejectDuplicateSemanticIdentities(t *testing.T) {
	tag := exampleRecord(t, "tag_definition")
	renamed := replaceField(tag, "name", mustText(t, "renamed"))
	require.ErrorContains(t, validateCollection(
		[]Value{Nested(tag), Nested(renamed)},
		collectionAttachedRecord,
		"enrollment_baseline.attachments",
	), "duplicate canonical collection key")

	firstChange := exampleRecord(t, "topology_change")
	secondChange := replaceField(firstChange, "post", Nested(exampleRecord(t, "topology_node")))
	require.ErrorContains(t, validateCollection(
		[]Value{Nested(firstChange), Nested(secondChange)},
		collectionTopologyChange,
		"topology_delta.changes",
	), "duplicate canonical collection key")

	firstEffect := exampleRecord(t, "path_effect")
	newPath := replaceField(exampleRecord(t, "path_state"), "path", Bytes([]byte("new")))
	secondEffect := replaceField(firstEffect, "new", Nested(newPath))
	require.ErrorContains(t, validateCollection(
		[]Value{Nested(firstEffect), Nested(secondEffect)},
		collectionPathEffect,
		"path_effect_list.effects",
	), "duplicate canonical collection key")
}

type fieldValue struct {
	name  string
	value Value
}

func nestedWith(t *testing.T, kind, name string, value Value) Value {
	t.Helper()
	return nestedWithFields(t, kind, fieldValue{name: name, value: value})
}

func nestedWithFields(t *testing.T, kind string, fields ...fieldValue) Value {
	t.Helper()
	record := exampleRecord(t, kind)
	for _, field := range fields {
		record = replaceField(record, field.name, field.value)
	}
	return Nested(record)
}
