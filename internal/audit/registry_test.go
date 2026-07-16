package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditRegistryCoversFrozenMetadataV1Kinds(t *testing.T) {
	expected := []string{
		"allocation_entry", "allocation_genesis", "attached_metadata_change", "attached_metadata_delta",
		"attached_metadata_genesis", "audit_event", "baseline_binding", "canonical_mutation",
		"content_version", "enrollment_baseline", "event", "event_identity", "ingest", "ingest_identity",
		"known_origin", "member_state", "member_state_change", "path_effect", "path_effect_list", "path_state",
		"preview_token", "provenance", "provenance_identity", "provenance_identity_ref", "scope_chain_entry",
		"tag_assignment", "tag_assignment_identity", "tag_definition", "tag_definition_identity",
		"topology_change", "topology_delta", "topology_genesis", "topology_node", "unknown_origin",
		"witness", "witness_change", "witness_change_list", "witnessed_state",
	}
	actual := make([]string, 0, len(recordSchemas))
	for kind := range recordSchemas {
		actual = append(actual, kind)
	}
	slices.Sort(actual)
	assert.Equal(t, expected, actual)
}

func TestAuditRegistryAcceptsEveryRegisteredShape(t *testing.T) {
	for kind := range recordSchemas {
		t.Run(kind, func(t *testing.T) {
			record := exampleRecord(t, kind)
			require.NoError(t, Validate(record))
			_, err := Encode(record)
			require.NoError(t, err)
		})
	}
}

func TestAuditRegistryAcceptsEveryRegisteredEnumAndNestedKind(t *testing.T) {
	for kind, registered := range recordSchemas {
		for _, registeredField := range registered.fields {
			for _, code := range registeredField.rule.textValues {
				t.Run(kind+"/"+registeredField.name+"/"+code, func(t *testing.T) {
					record := replaceField(exampleRecord(t, kind), registeredField.name, mustText(t, code))
					require.NoError(t, Validate(record))
				})
			}
			for _, nestedKind := range nestedKindVariants(registeredField.rule) {
				t.Run(kind+"/"+registeredField.name+"/"+nestedKind, func(t *testing.T) {
					value := Nested(exampleRecord(t, nestedKind))
					if registeredField.rule.typeOf == typeList {
						value = List(value)
					}
					record := replaceField(exampleRecord(t, kind), registeredField.name, value)
					require.NoError(t, Validate(record))
				})
			}
		}
	}
}

func TestAuditRegistryRejectsUnknownAndInexactShapes(t *testing.T) {
	valid := exampleRecord(t, "tag_definition")
	tests := map[string]Record{
		"unknown record": {Kind: "future_record"},
		"missing field": {
			Kind:   valid.Kind,
			Fields: valid.Fields[:1],
		},
		"extra field": {
			Kind:   valid.Kind,
			Fields: append(slices.Clone(valid.Fields), Field{Name: "future", Value: Unsigned(1)}),
		},
		"duplicate field": {
			Kind:   valid.Kind,
			Fields: append(slices.Clone(valid.Fields), valid.Fields[0]),
		},
		"wrong type":      replaceField(valid, "tag_id", Unsigned(1)),
		"required absent": replaceField(valid, "name", Absent()),
	}
	for name, record := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, Validate(record))
			_, err := Encode(record)
			require.Error(t, err)
		})
	}
}

func TestAuditRegistryEnforcesStaticTextCodes(t *testing.T) {
	tests := []struct {
		kind  string
		field string
	}{
		{kind: "allocation_entry", field: "has_audited_mutation"},
		{kind: "attached_metadata_change", field: "record_kind"},
		{kind: "audit_event", field: "event_kind"},
		{kind: "audit_event", field: "origin"},
		{kind: "content_version", field: "transition_kind"},
		{kind: "path_state", field: "state"},
		{kind: "topology_node", field: "node_kind"},
		{kind: "topology_node", field: "state"},
		{kind: "witness_change", field: "action"},
	}
	for _, test := range tests {
		t.Run(test.kind+"/"+test.field, func(t *testing.T) {
			record := exampleRecord(t, test.kind)
			if test.field == "has_audited_mutation" {
				require.Error(t, Validate(replaceField(record, test.field, mustText(t, "true"))))
				return
			}
			require.Error(t, Validate(replaceField(record, test.field, mustText(t, "future_code"))))
		})
	}
}

func TestAuditRegistryValidatesOptionalAndNestedFields(t *testing.T) {
	for kind, registered := range recordSchemas {
		for _, registeredField := range registered.fields {
			if !registeredField.rule.optional {
				continue
			}
			t.Run(kind+"/"+registeredField.name, func(t *testing.T) {
				record := exampleRecord(t, kind)
				require.NoError(t, Validate(record))
				presentRule := registeredField.rule
				presentRule.optional = false
				record = replaceField(record, registeredField.name, exampleValue(t, presentRule))
				require.NoError(t, Validate(record))
			})
		}
	}

	witness := exampleRecord(t, "witnessed_state")
	witness = replaceField(witness, "node", Nested(exampleRecord(t, "known_origin")))
	require.ErrorContains(t, Validate(witness), "disallowed")

	malformedNode := exampleRecord(t, "topology_node")
	malformedNode.Fields = malformedNode.Fields[:1]
	witness = replaceField(exampleRecord(t, "witnessed_state"), "node", Nested(malformedNode))
	require.ErrorContains(t, Validate(witness), "missing field")

	event := replaceField(exampleRecord(t, "audit_event"), "attachment_identity",
		Nested(exampleRecord(t, "ingest_identity")))
	require.ErrorContains(t, Validate(event), "disallowed")
}

func TestAuditRegistryRegisteredGoldenHashes(t *testing.T) {
	expected := map[string]string{
		"allocation_entry":          "ad35cdd67f7cbdca45ec9c7557c32d4493f0e4df8357b7bc0a50a4513eef3c55",
		"allocation_genesis":        "5e9d1f128e743cd3ee0df3448b57356e9d4a35c2292eaedf2850846229aebe80",
		"attached_metadata_change":  "5b889936862fcf242e37260eb53ac40f26618d5fc8019a1ecebfc22a0afcd13e",
		"attached_metadata_delta":   "15477e3808f187bd2ee752bd48619b49649b22d01c008eb766e9bae8b9f7738b",
		"attached_metadata_genesis": "87a79f8c58ea993892e97db2d65b26107381de6211781fb23c1c06128011d354",
		"audit_event":               "d94906b15e34b4fa51344e843d45c5e5d584f5db7252e4f1a4c6aaaf41e01885",
		"baseline_binding":          "e6a91c8a8446946727344f568e2ce5a3acbf01887ea4acc582c406557d478e14",
		"canonical_mutation":        "ba90467069c4477c40aa308252fee4fc5915a22f62a4b4d5768941814150c3b1",
		"content_version":           "a26828acdb01d692e69ce1f2f49e6cab9dfdbe9e9c4270293ee4c47fb70c961d",
		"enrollment_baseline":       "c25b754ff40dce7c87379e5b17ae3c8bca387c6823888049cce8a0898fd87b30",
		"event":                     "1b90a08f1b177924abb4c73b6766080013dd099f4795e21cfad806b19e72ad4b",
		"event_identity":            "5ee82a22ac305d21961a6646836cad10e73d1dd19e72d607566d0ea0685e70a5",
		"ingest":                    "68d5f41b0353d9e3d965c2a645a1733b4b43e89b347ba6eb09e02f8a9903e82e",
		"ingest_identity":           "b36f958aa06ee224f413ee8d20ac58860507c78b89b807602cecb16ab97dbb9d",
		"known_origin":              "4ef50f1b9cc97af64788bb76bbe678ed2513e90077a9453b6223b0d9cb0a39c1",
		"member_state":              "8d82845e3a5b381440a6853b883a7e3b75bcd184342a08dd4fb69db3dbdf751f",
		"member_state_change":       "c57d08354146220824e0517be4c021829b2691b78aa906ca2223ba0a85ce0b4b",
		"path_effect":               "e3a3edd499f4b82a44b52c1470172ae8b36ec452eebb8069b6f600bdc7b30997",
		"path_effect_list":          "d6a0e010be748988dffe32e19e031b3a56b0f216aae114b38d00350667b0c3b4",
		"path_state":                "bb358f2812f633c0139c639d0d9e1990a859bbb261f7825c30f00a42c5c10eda",
		"preview_token":             "914b97b5a1f21c7c9336c35abd9e9ae4e3829b1e7c487e3fb2622bcd650c3372",
		"provenance":                "a6ce16f925767af1cb677963002b7403dd0a3ed419f68252c210cd3d68d00c54",
		"provenance_identity":       "90f92e5ea3e1b173414d4380f76114ff78431f3075e29cec20746465a2d48a72",
		"provenance_identity_ref":   "05cc34a929455b3c8909c311ef2ae7d0ef5302c63a402708b40e0a855324b4d9",
		"scope_chain_entry":         "e5eb564cf6632e574720f1a72ff4c8ed1717acf98acc0cb79b778ba6cc6c4a2b",
		"tag_assignment":            "a28d9b6237cec86632df1241cfc6abeb98fc54084827b7f56ba7c5e7ef4d6805",
		"tag_assignment_identity":   "afa085f52694144504b355e4927407171843228873f46700552fc4667c5016e4",
		"tag_definition":            "24fa83c5e4344d70b43c5478af58f292e17340e7d0ebe5852320ebd809641db2",
		"tag_definition_identity":   "e32925ac70e10a1154c275477c587d5f9994aa44386ecd09ca7a23aef9b6cce1",
		"topology_change":           "c268193d2565130a5d2808f868561c97c8b2b5a20592bde7ee33815dc4d13b58",
		"topology_delta":            "098688d0a0306900d33e1487a97349444347bf438ff82d2929c6d8ebdc07d899",
		"topology_genesis":          "da96cc19d880e1f29ce729842389036193816cbf9f86e3341d41b2cc520792a1",
		"topology_node":             "7b4f04dffd8225a6035f8c90eb07f71aa439c162e766a0823bf46d8d4556fefc",
		"unknown_origin":            "d8fbfcf127bdca2cb544572e7f769001466363b385e46ea47edd37469c2e8e08",
		"witness":                   "de552ab7b3738eb1cd3630bdd94ebba8d87a29f458f212e404362b6309e2e97f",
		"witness_change":            "9d5d48e12013fe0176cde8bf643f72c93869566755763e9ef266d7a55f8b202b",
		"witness_change_list":       "9434b863354252dd98311ff0cfda38a57061d08fbad6eda3b10a70e029c1a951",
		"witnessed_state":           "f70fbacc0d76f39df805ca4d46b88aa2b05332b3cd86fe494042cb70e3de5487",
	}
	for kind := range recordSchemas {
		digest, err := Hash(exampleRecord(t, kind))
		require.NoError(t, err)
		actual := hex.EncodeToString(digest[:])
		assert.Equal(t, expected[kind], actual, kind)
	}
}

func exampleRecord(t *testing.T, kind string) Record {
	t.Helper()
	registered, ok := recordSchemas[kind]
	require.True(t, ok, kind)
	record := Record{Kind: kind, Fields: make([]Field, 0, len(registered.fields))}
	for _, registeredField := range registered.fields {
		record.Fields = append(record.Fields, Field{
			Name:  registeredField.name,
			Value: exampleValue(t, registeredField.rule),
		})
	}
	return record
}

func exampleValue(t *testing.T, rule valueRule) Value {
	t.Helper()
	if rule.optional || rule.typeOf == typeAbsent {
		return Absent()
	}
	switch rule.typeOf {
	case typeBool:
		return Bool(true)
	case typeUnsigned:
		return Unsigned(1)
	case typeSigned:
		return Signed(-1)
	case typeBytes:
		return Bytes([]byte{0x00, 0xff})
	case typeText:
		if len(rule.textValues) != 0 {
			return mustText(t, rule.textValues[0])
		}
		return mustText(t, "sample")
	case typeTimestamp:
		return mustTimestamp(t, "2026-07-16T12:34:56.123456789Z")
	case typeUUID:
		return mustUUID(t, "00112233-4455-4677-8899-aabbccddeeff")
	case typeDigest:
		return Digest(sha256.Sum256([]byte("sample")))
	case typeList:
		return List(exampleValue(t, *rule.listElement))
	case typeRecord:
		require.NotEmpty(t, rule.recordKinds)
		return Nested(exampleRecord(t, rule.recordKinds[0]))
	default:
		require.FailNow(t, fmt.Sprintf("unsupported example rule %d", rule.typeOf))
		return Value{}
	}
}

func replaceField(record Record, name string, value Value) Record {
	record = cloneRecord(record)
	for index := range record.Fields {
		if record.Fields[index].Name == name {
			record.Fields[index].Value = value
			return record
		}
	}
	panic("field not found: " + name)
}

func nestedKindVariants(rule valueRule) []string {
	if rule.typeOf == typeRecord {
		return rule.recordKinds
	}
	if rule.typeOf == typeList && rule.listElement.typeOf == typeRecord {
		return rule.listElement.recordKinds
	}
	return nil
}
