package document

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrimarySelectionSkipsUnreliableKinds(t *testing.T) {
	events := []DocumentEventV1{{DateKind: "accessed"}, {DateKind: "printed"}, {DateKind: "observed"}, {DateKind: "exported"}}
	_, ok := SelectPrimaryEvent(events, "other", "vault", "full")
	require.False(t, ok)
}

func TestPrimarySelectionOrders(t *testing.T) {
	tests := []struct {
		kind  DocumentKind
		order []DateKind
	}{
		{"email", []DateKind{"sent", "received", "created", "modified", "document_date", "imported", "vault_recorded"}},
		{"message", []DateKind{"sent", "received", "authored", "created", "modified", "document_date", "imported", "vault_recorded"}},
		{"calendar", []DateKind{"started", "ended", "authored", "created", "modified", "document_date", "imported", "vault_recorded"}},
		{"image", []DateKind{"captured", "created", "authored", "modified", "document_date", "imported", "vault_recorded"}},
		{"audio_video", []DateKind{"captured", "authored", "created", "modified", "produced", "document_date", "imported", "vault_recorded"}},
		{"other", []DateKind{"authored", "created", "modified", "produced", "document_date", "imported", "vault_recorded"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			for i, want := range tt.order {
				var events []DocumentEventV1
				for _, kind := range tt.order[i:] {
					events = append(events, DocumentEventV1{EventID: string(kind), DateKind: kind})
				}
				slices.Reverse(events)
				got, ok := SelectPrimaryEvent(events, tt.kind, "vault", "full")
				require.True(t, ok)
				require.Equal(t, string(want), got.EventID)
				require.NotEmpty(t, got.Reason)
			}
		})
	}
}

func TestPrimarySelectionDisclosure(t *testing.T) {
	events := []DocumentEventV1{
		{EventID: "secret", DateKind: "sent", SourceKey: "metadata/gen/sent", EvidenceKind: "source_metadata", Sensitive: true},
		{EventID: "original", DateKind: "received", SourceKey: "metadata/gen/received", EvidenceKind: "source_metadata"},
	}
	for _, tt := range []struct{ disclosure, want string }{{"safe", "original"}, {"full", "secret"}} {
		got, ok := SelectPrimaryEvent(events, "email", "vault", tt.disclosure)
		require.True(t, ok)
		require.Equal(t, tt.want, got.EventID)
		require.Equal(t, tt.disclosure, got.Disclosure)
	}
	for _, tt := range []struct{ scope, disclosure string }{{"", "full"}, {"collection", "full"}, {"vault", ""}, {"vault", "redacted"}} {
		_, ok := SelectPrimaryEvent(events, "email", tt.scope, tt.disclosure)
		require.False(t, ok, "%+v", tt)
	}
}

func TestPrimarySelectionReliabilityBeforeResolution(t *testing.T) {
	base := DocumentEventV1{EventID: "preferred", DateKind: "sent", SourceKey: "z", ClaimBasis: "source_asserted", ParseConfidence: "exact", Precision: "date"}
	tests := []struct {
		name             string
		preferred, other DocumentEventV1
	}{
		{"interpretation", base, DocumentEventV1{EventID: "supplied", DateKind: "sent", ClaimBasis: "source_asserted", ParseConfidence: "profile_interpreted", Precision: "fraction", UTCKey: "2020-01-01T00:00:00.000000000Z"}},
		{"observation", DocumentEventV1{EventID: "preferred", DateKind: "sent", ParseConfidence: "profile_interpreted"}, DocumentEventV1{EventID: "observed", DateKind: "sent", ClaimBasis: "docbank_observed", ParseConfidence: "exact", UTCKey: "instant"}},
		{"instant", DocumentEventV1{EventID: "preferred", DateKind: "sent", UTCKey: "instant", Precision: "minute"}, DocumentEventV1{EventID: "floating", DateKind: "sent", Precision: "fraction"}},
		{"precision", DocumentEventV1{EventID: "preferred", DateKind: "sent", Precision: "second"}, DocumentEventV1{EventID: "coarse", DateKind: "sent", Precision: "minute"}},
		{"source", DocumentEventV1{EventID: "preferred", DateKind: "sent", SourceKey: "a"}, DocumentEventV1{EventID: "other", DateKind: "sent", SourceKey: "b"}},
		{"event", DocumentEventV1{EventID: "a", DateKind: "sent"}, DocumentEventV1{EventID: "b", DateKind: "sent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []DocumentEventV1{tt.other, tt.preferred}
			before := slices.Clone(events)
			for range 2 {
				got, ok := SelectPrimaryEvent(events, "email", "vault", "full")
				require.True(t, ok)
				require.Equal(t, tt.preferred.EventID, got.EventID)
				require.Equal(t, before, events)
				slices.Reverse(events)
				slices.Reverse(before)
			}
		})
	}
}
