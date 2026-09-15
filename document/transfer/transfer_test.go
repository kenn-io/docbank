package transfer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClosedVocabulariesAreExactlyTheSpecifiedMembers(t *testing.T) {
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"record types", len(AllRecordTypes()), 7},
		{"kinds", len(AllKinds()), 9},
		{"source types", len(AllSourceTypes()), 18},
		{"date kinds", len(AllDateKinds()), 14},
		{"precisions", len(AllPrecisions()), 5},
		{"timezone kinds", len(AllTimezoneKinds()), 5},
		{"origins", len(AllOrigins()), 13},
		{"envelope roles", len(AllEnvelopeRoles()), 8},
		{"scope directions", len(AllScopeDirections()), 3},
		{"directions", len(AllDirections()), 3},
		{"record origins", len(AllRecordOrigins()), 2},
		{"capabilities", len(AllCapabilities()), 10},
		{"capability states", len(AllCapabilityStates()), 5},
		{"reason codes", len(AllReasonCodes()), 23},
		{"availabilities", len(AllAvailabilities()), 7},
		{"attachment roles", len(AllAttachmentRoles()), 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, test.got)
		})
	}

	assert.False(t, ValidKind("message"))
	assert.True(t, ValidKind(KindAttachmentOccurrence))
	assert.False(t, ValidPrecision("day"))
	assert.False(t, ValidEnvelopeRole("reply_to"))
	assert.False(t, ValidAttachmentRole("banner"))
	assert.True(t, ValidRecordOrigin(RecordOriginProducerCanonical))
	assert.Equal(t, []ContactPointKind{
		ContactPointKindEmail, ContactPointKindPhone, ContactPointKindAppleID,
		ContactPointKindWhatsApp, ContactPointKindHandle, ContactPointKindURI,
	}, AllContactPointKinds())
	assert.Equal(t, []CustodianRank{CustodianRankPrimary, CustodianRankAdditional}, AllCustodianRanks())
	assert.Equal(t, []HistoryState{HistoryStateObserved, HistoryStateAbsent, HistoryStateUnsupported}, AllHistoryStates())
	assert.True(t, ValidContactPointKind(ContactPointKindAppleID))
	assert.False(t, ValidCustodianRank("owner"))
	assert.True(t, ValidHistoryState(HistoryStateUnsupported))

	kinds := AllKinds()
	kinds[0] = "mutated"
	assert.True(t, ValidKind(KindEmail), "callers must receive a fresh vocabulary slice")
}

func TestRecordEnvelopeCarriesEverySpecifiedField(t *testing.T) {
	typeOf := reflect.TypeFor[RecordV1]()
	got := make([]string, 0, typeOf.NumField())
	for field := range typeOf.Fields() {
		got = append(got, strings.Split(field.Tag.Get("json"), ",")[0])
	}
	require.ElementsMatch(t, []string{
		"record_type", "record_ref", "legacy_ref", "kind", "source_ref",
		"conversation_ref", "parent_record_ref", "revision", "dates", "direction",
		"participants", "custodian", "subject", "body_text", "body_media_type",
		"body_truncated", "raw", "normalized_sha256", "record_origin", "attachments",
		"attachment", "history", "deleted_from_source", "source_fields",
	}, got)
}

func TestRecordKeySeparatesDelimiterCollisions(t *testing.T) {
	a, err := RecordKey("account/chat_message/x", KindChatMessage, "y")
	require.NoError(t, err)
	b, err := RecordKey("account", KindChatMessage, "x/chat_message/y")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	require.Len(t, a, 64)

	_, err = RecordKey("", KindChatMessage, "record")
	require.Error(t, err)
	_, err = RecordKey("source", "message", "record")
	require.Error(t, err)
	_, err = RecordKey(strings.Repeat("s", MaxSourceRefBytes+1), KindChatMessage, "record")
	require.Error(t, err)
}
