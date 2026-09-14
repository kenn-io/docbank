package document

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type actorKeyVector struct {
	Display       string `json:"display"`
	Input         string `json:"input"`
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Normalization string `json:"normalization"`
	Normalized    string `json:"normalized"`
}

func validDocumentEvents() DocumentEventsV1 {
	digest := strings.Repeat("a", 64)
	event := DocumentEventV1{
		Actors: []DocumentEventActorV1{{
			ActorKey: "email:ada@example.test", Address: "ada@example.test", Claim: "Ada",
			DisplayName: "Ada", Ordinal: 0, Role: "author",
		}},
		AxisKey: "2019-03-04T00:00:00.000000000", ClaimBasis: "source_asserted",
		DateKind: "created", DateValue: "2019-03-04", EvidenceID: "metadata/gen-1",
		EvidenceKind: "source_metadata", EvidenceLocator: "field:created", EvidenceSHA256: digest,
		ParseConfidence: "exact", Precision: "date", RawValue: "2019-03-04",
		SourceKey: "metadata/gen-1/created", TimezoneKind: "omitted",
	}
	event.EventID = DocumentEventID("vault-uid-1", "cv-1", event.SourceKey, event.DateKind,
		event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	return DocumentEventsV1{
		VaultUID: "vault-uid-1", ContentVersionID: "cv-1", ContractVersion: DocumentEventsContractV1,
		DocumentKind: "other", Diagnostics: []DocumentEventDiagnosticV1{},
		Events: []DocumentEventV1{event}, Primaries: []DocumentEventPrimaryV1{{
			EventID: event.EventID, Reason: "created", RuleID: "primary-rule/v1", ScopeClass: "vault", Disclosure: "full",
		}}, Sources: []DocumentEventSourceV1{{EvidenceID: event.EvidenceID, EvidenceKind: event.EvidenceKind, EvidenceSHA256: digest}},
	}
}

func TestDocumentEventsCodecRejectsNonCanonicalBytes(t *testing.T) {
	value := validDocumentEvents()
	encoded, checksum, err := MarshalDocumentEventsV1(value)
	require.NoError(t, err)
	decoded, decodedChecksum, err := DecodeDocumentEventsV1(encoded)
	require.NoError(t, err)
	assert.Equal(t, value, decoded)
	assert.Equal(t, checksum, decodedChecksum)

	_, _, err = DecodeDocumentEventsV1(bytes.Replace(encoded, []byte(`"events"`), []byte(`"Events"`), 1))
	require.ErrorContains(t, err, "decoding document events")
}

func TestDocumentEventsCodecCanonicalEmptyRecord(t *testing.T) {
	value := DocumentEventsV1{VaultUID: "vault-uid-1", ContentVersionID: "cv-1", ContractVersion: DocumentEventsContractV1,
		DocumentKind: "other", Diagnostics: []DocumentEventDiagnosticV1{}, Events: []DocumentEventV1{},
		Primaries: []DocumentEventPrimaryV1{}, Sources: []DocumentEventSourceV1{}}
	encoded, _, err := MarshalDocumentEventsV1(value)
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte(`{"content_version_id":"cv-1","contract_version":"document-events/v1","diagnostics":[],"document_kind":"other","events":[],"primaries":[],"sources":[],"vault_uid":"vault-uid-1"}`), encoded), "canonical bytes differ: %s", encoded)
	reordered := bytes.Replace(encoded,
		[]byte(`"content_version_id":"cv-1","contract_version":"document-events/v1"`),
		[]byte(`"contract_version":"document-events/v1","content_version_id":"cv-1"`), 1)
	require.NotEqual(t, encoded, reordered)
	_, _, err = DecodeDocumentEventsV1(reordered)
	require.ErrorContains(t, err, "bytes are not canonical")
}

func TestDocumentEventsCodecRejectsMalformedAndNonCanonicalJSON(t *testing.T) {
	encoded, _, err := MarshalDocumentEventsV1(validDocumentEvents())
	require.NoError(t, err)
	cases := map[string][]byte{
		"unknown field":   bytes.Replace(encoded, []byte(`"vault_uid"`), []byte(`"unknown":"x","vault_uid"`), 1),
		"duplicate field": bytes.Replace(encoded, []byte(`"vault_uid"`), []byte(`"vault_uid":"other","vault_uid"`), 1),
		"trailing bytes":  append(append([]byte{}, encoded...), '\n'),
		"invalid UTF-8":   append(append([]byte{}, encoded[:len(encoded)-1]...), 0xff, '}'),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) { _, _, err := DecodeDocumentEventsV1(input); require.Error(t, err) })
	}
}

func TestDocumentEventsCodecRejectsContractViolations(t *testing.T) {
	tests := map[string]func(*DocumentEventsV1){
		"bad event id":         func(v *DocumentEventsV1) { v.Events[0].EventID = strings.Repeat("b", 64) },
		"bad axis key":         func(v *DocumentEventsV1) { v.Events[0].AxisKey = "2019" },
		"bad fraction count":   func(v *DocumentEventsV1) { v.Events[0].FractionDigits = 1 },
		"unknown role":         func(v *DocumentEventsV1) { v.Events[0].Actors[0].Role = "owner" },
		"malformed actor key":  func(v *DocumentEventsV1) { v.Events[0].Actors[0].ActorKey = "email:Ada@example.test" },
		"duplicate actor slot": func(v *DocumentEventsV1) { v.Events[0].Actors = append(v.Events[0].Actors, v.Events[0].Actors[0]) },
		"duplicate event slot": func(v *DocumentEventsV1) { v.Events = append(v.Events, v.Events[0]) },
		"missing source":       func(v *DocumentEventsV1) { v.Sources = []DocumentEventSourceV1{} },
		"unknown primary":      func(v *DocumentEventsV1) { v.Primaries[0].EventID = strings.Repeat("b", 64) },
		"duplicate primary":    func(v *DocumentEventsV1) { v.Primaries = append(v.Primaries, v.Primaries[0]) },
		"safe sensitive":       func(v *DocumentEventsV1) { v.Primaries[0].Disclosure = "safe"; v.Events[0].Sensitive = true },
		"safe sensitive actor": func(v *DocumentEventsV1) { v.Primaries[0].Disclosure = "safe"; v.Events[0].Actors[0].Sensitive = true },
		"invalid UTF-8 value":  func(v *DocumentEventsV1) { v.Events[0].RawValue = string([]byte{0xff}) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validDocumentEvents()
			mutate(&value)
			_, _, err := MarshalDocumentEventsV1(value)
			require.Error(t, err)
		})
	}
}

func TestDocumentEventsCodecSourcesHaveIndependentByteBudget(t *testing.T) {
	value := validDocumentEvents()
	// Actor-only and diagnostic evidence can add sources without adding dates.
	for index := range MaxDocumentEvents {
		source := value.Sources[0]
		source.EvidenceID = "extra/" + strconv.Itoa(index)
		value.Sources = append(value.Sources, source)
	}
	encoded, _, err := MarshalDocumentEventsV1(value)
	require.NoError(t, err)
	decoded, _, err := DecodeDocumentEventsV1(encoded)
	require.NoError(t, err)
	require.Len(t, decoded.Sources, MaxDocumentEvents+1)

	value.Sources = make([]DocumentEventSourceV1, MaxDocumentEventsEncodedBytes/64+1)
	_, _, err = MarshalDocumentEventsV1(value)
	require.ErrorContains(t, err, "too many child records")
}

func TestDocumentEventsCodecRejectsInvalidPrimaryScopes(t *testing.T) {
	for _, scope := range []string{"", "collection"} {
		t.Run(scope, func(t *testing.T) {
			value := validDocumentEvents()
			value.Primaries[0].ScopeClass = scope
			_, _, err := MarshalDocumentEventsV1(value)
			require.ErrorContains(t, err, "invalid scope class")
		})
	}
}

func TestDocumentEventsCodecBoundsAndDoesNotMutateInput(t *testing.T) {
	value := validDocumentEvents()
	value.Events[0].RawValue = strings.Repeat("x", MaxDocumentEventRawValueBytes+1)
	_, _, err := MarshalDocumentEventsV1(value)
	require.Error(t, err)

	value = validDocumentEvents()
	second := value.Events[0]
	second.SourceKey = "a"
	second.EventID = DocumentEventID(value.VaultUID, value.ContentVersionID, second.SourceKey, second.DateKind,
		second.DateValue, second.Precision, second.TimezoneKind, second.EvidenceSHA256)
	value.Events = append(value.Events, second)
	before := value.Events[0].SourceKey
	_, _, err = MarshalDocumentEventsV1(value)
	require.NoError(t, err)
	assert.Equal(t, before, value.Events[0].SourceKey)
	assert.Equal(t, "author", string(value.Events[0].Actors[0].Role))

	_, _, err = DecodeDocumentEventsV1(bytes.Repeat([]byte(" "), MaxDocumentEventsEncodedBytes+1))
	require.Error(t, err)
}

func TestDocumentEventsCodecRejectsCollectionAndChildBounds(t *testing.T) {
	tests := map[string]func(*DocumentEventsV1){
		"event count": func(v *DocumentEventsV1) { v.Events = make([]DocumentEventV1, MaxDocumentEvents+1) },
		"actor count": func(v *DocumentEventsV1) { v.Events[0].Actors = make([]DocumentEventActorV1, MaxDocumentEventActors+1) },
		"actor claim": func(v *DocumentEventsV1) {
			v.Events[0].Actors[0].Claim = strings.Repeat("x", MaxDocumentEventActorClaimBytes+1)
		},
		"locator": func(v *DocumentEventsV1) {
			v.Events[0].EvidenceLocator = strings.Repeat("x", MaxDocumentEventLocatorBytes+1)
		},
		"diagnostic count": func(v *DocumentEventsV1) {
			v.Diagnostics = make([]DocumentEventDiagnosticV1, MaxDocumentEventsEncodedBytes/32+1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validDocumentEvents()
			mutate(&value)
			_, _, err := MarshalDocumentEventsV1(value)
			require.Error(t, err)
		})
	}
}

func TestActorKeyGoldenVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "actor_keys.json"))
	require.NoError(t, err)
	var vectors []actorKeyVector
	require.NoError(t, json.Unmarshal(raw, &vectors, json.RejectUnknownMembers(true)))
	require.Len(t, vectors, 8)
	for _, vector := range vectors {
		t.Run(vector.Kind+"/"+vector.Input, func(t *testing.T) {
			key, err := ActorKeyV1(vector.Kind, vector.Input)
			if vector.Key == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, vector.Key, key)
			assert.Equal(t, vector.Kind+":"+vector.Normalized, key)
			assert.LessOrEqual(t, len(key), MaxActorKeyBytes)
		})
	}
	_, err = ActorKeyV1("address", "x@y.test")
	require.ErrorContains(t, err, "actor key kind is unknown")
	_, err = ActorKeyV1("email", "")
	require.ErrorContains(t, err, "actor key value is empty")
	_, err = ActorKeyV1("email", "x@[127.0.0.1]")
	require.ErrorContains(t, err, "domain is a literal")
	_, err = ActorKeyV1("handle", string([]byte{'x', '/', 0xff}))
	require.ErrorContains(t, err, "not valid UTF-8")
}
