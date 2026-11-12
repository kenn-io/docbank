package api

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestWhenFieldTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		claim document.DocumentEventV1
		want  string
	}{
		{"offset", document.DocumentEventV1{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "offset", ZoneText: "-04:00", UTCKey: "2026-03-10T13:12:00.000000000Z", OffsetSeconds: new(-14400)}, `{"state":"exact","precision":"minute","at":"2026-03-10T13:12:00Z","local":"2026-03-10T09:12:00","zone_state":"offset","source_zone":"-04:00","source_offset_seconds":-14400}`},
		{"producer UTC", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T23:50:00", TimezoneKind: "utc", ZoneText: "Z", UTCKey: "2026-03-10T23:50:00.000000000Z"}, `{"state":"exact","precision":"second","at":"2026-03-10T23:50:00Z","local":"2026-03-10T23:50:00","zone_state":"utc","source_zone":"Z","source_offset_seconds":0}`},
		{"omitted", document.DocumentEventV1{Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "omitted", ZoneText: "-0000"}, `{"state":"floating","precision":"minute","local":"2026-03-10T09:12:00","zone_state":"omitted"}`},
		{"missing offset zone", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T09:12:00", TimezoneKind: "offset", OffsetSeconds: new(3600), UTCKey: "2026-03-10T08:12:00.000000000Z"}, `{"state":"exact","precision":"second","at":"2026-03-10T08:12:00Z","zone_state":"unknown"}`},
		{"missing named zone", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T09:12:00", TimezoneKind: "named"}, `{"state":"floating","precision":"second","local":"2026-03-10T09:12:00","zone_state":"omitted"}`},
		{"date", document.DocumentEventV1{Precision: "date", DateValue: "2026-03-10"}, `{"state":"date","precision":"date","date":"2026-03-10"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response := versionDatesAPI(store.DocumentDateFacts{State: "published", View: store.DocumentEventView{Events: document.DocumentEventsV1{Events: []document.DocumentEventV1{tc.claim}}}}, "safe", "")
			raw, err := json.Marshal(response.Claims[0].When)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			want["placed_by"] = map[string]any{"source": string(tc.claim.DateKind), "rule": "primary-rule/v1", "fallback": tc.claim.DateKind == "imported" || tc.claim.DateKind == "vault_recorded", "event_id": tc.claim.EventID, "claim_basis": tc.claim.ClaimBasis}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %s; want %s", raw, tc.want)
			}
		})
	}
}

func TestVersionDatesProjectionUsesSelectedDisclosure(t *testing.T) {
	t.Parallel()
	sent := document.DocumentEventV1{EventID: "sent", DateKind: "sent", Precision: "date", DateValue: "2026-03-10", Actors: []document.DocumentEventActorV1{{Role: "sender"}, {Role: "blind_copy", Sensitive: true}}}
	created := document.DocumentEventV1{EventID: "created", DateKind: "created", Precision: "date", DateValue: "2026-03-09"}
	f := store.DocumentDateFacts{Version: store.ContentVersion{ID: "version", NodeID: 1}, State: "published", View: store.DocumentEventView{Events: document.DocumentEventsV1{DocumentKind: "email", Events: []document.DocumentEventV1{sent, created}, Primaries: []document.DocumentEventPrimaryV1{{EventID: "created", ScopeClass: "vault", Disclosure: "safe"}, {EventID: "sent", ScopeClass: "vault", Disclosure: "full"}}}}}
	safe := versionDatesAPI(f, "safe", "")
	if len(safe.Claims) != 1 || safe.Claims[0].EventID != "created" || !safe.Claims[0].Primary {
		t.Fatalf("safe = %+v", safe)
	}
	full := versionDatesAPI(f, "full", "")
	if len(full.Claims) != 2 || !full.Claims[0].Primary {
		t.Fatalf("full = %+v", full)
	}
}

func TestVersionPeopleProjectionHidesBccAndUnsupportedPeople(t *testing.T) {
	t.Parallel()
	f := store.DocumentPeopleFacts{Version: store.ContentVersion{ID: "version", NodeID: 1}, State: "published", Persons: map[string]store.FactPerson{"visible": {Person: store.Person{PersonID: "visible", DisplayName: "Visible"}, Visible: true}, "hidden": {Person: store.Person{PersonID: "hidden", DisplayName: "Hidden"}}}, Edges: []document.DocumentPersonEdgeV1{{PersonID: "visible", Role: "author"}, {PersonID: "hidden", Role: "author"}, {PersonID: "visible", Role: "blind_copy", Sensitive: true}}, Assertions: []store.PersonDocumentAssertion{{PersonID: "hidden", Role: "author"}}, Candidates: []store.VersionCandidate{{CandidateID: "candidate", Occurrences: []store.PersonCandidateOccurrence{{Role: "blind_copy"}, {Role: "author"}}}}}
	f.Persons["visible"] = store.FactPerson{Person: store.Person{PersonID: "visible", DisplayName: "Private Bcc", State: "provisional", Origin: "derived"}, Visible: true}
	f.Edges[0].RawLabel = "Public Author"
	// The store supplies already-filtered candidate occurrences for safe reads.
	safeFacts := f
	safeFacts.Edges = f.Edges[:2]
	safeFacts.Candidates = []store.VersionCandidate{{CandidateID: "candidate", Occurrences: []store.PersonCandidateOccurrence{{Role: "author"}}, OccurrenceLabels: map[store.PersonCandidateOccurrence]string{{Role: "author"}: "Recipient"}}}
	safe := versionPeopleAPI(safeFacts, true, "")
	if len(safe.Edges) != 1 || safe.Edges[0].PersonID != "visible" || safe.Edges[0].DisplayName != "Public Author" || len(safe.Assertions) != 0 || len(safe.Unresolved) != 1 || safe.Unresolved[0].Role != "author" || safe.Unresolved[0].RawLabel != "Recipient" {
		t.Fatalf("safe: %+v", safe)
	}
	// Only explicit assignments may use a provisional derived catalog name in safe reads.
	for _, tc := range []struct {
		name, evidence, basis, label, want string
	}{
		{"automatic", "source_metadata", "identifier_match", "Public Author", "Public Author"},
		{"automatic empty label", "source_metadata", "identifier_match", "", ""},
		{"operator assertion", "operator_assertion", "operator_assigned", "", "Private Bcc"},
		{"operator custodian", "custodian_assignment", "operator_assigned", "Assigned Label", "Private Bcc"},
		{"inferred custodian", "custodian_assignment", "package_column", "Package Label", "Package Label"},
		{"inferred custodian empty label", "custodian_assignment", "transfer_record", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edgeFacts := safeFacts
			edgeFacts.Edges = []document.DocumentPersonEdgeV1{{PersonID: "visible", EvidenceKind: tc.evidence, Basis: tc.basis, RawLabel: tc.label}}
			got := versionPeopleAPI(edgeFacts, true, "")
			if len(got.Edges) != 1 || got.Edges[0].DisplayName != tc.want || got.Edges[0].RawLabel != tc.label {
				t.Fatalf("safe edges = %+v; want name %q and label %q", got.Edges, tc.want, tc.label)
			}
		})
	}
	safeFacts.Persons["visible"] = store.FactPerson{Person: store.Person{PersonID: "visible", DisplayName: "Confirmed Name", State: "curated", Origin: "derived"}, Visible: true}
	if got := versionPeopleAPI(safeFacts, true, "").Edges[0].DisplayName; got != "Confirmed Name" {
		t.Fatalf("curated name = %q", got)
	}
	f.Candidates[0].OccurrenceLabels = map[store.PersonCandidateOccurrence]string{{Role: "blind_copy"}: "Blind", {Role: "author"}: "Recipient"}
	f.Persons["hidden"] = store.FactPerson{Person: store.Person{PersonID: "hidden", DisplayName: "Hidden"}, Visible: true}
	full := versionPeopleAPI(f, false, "")
	if len(full.Edges) != 3 || len(full.Assertions) != 1 || full.Assertions[0].DisplayName != "Hidden" || len(full.Unresolved) != 2 {
		t.Fatalf("full: %+v", full)
	}
}

func TestEmittedWhenMatchesContractExamples(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path  string
		claim document.DocumentEventV1
	}{
		{"example-1.json", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T23:40:00", TimezoneKind: "offset", ZoneText: "-04:00", OffsetSeconds: new(-14400), UTCKey: "2026-03-11T03:40:00.000000000Z"}},
		{"example-2.json", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T23:40:00", TimezoneKind: "offset", OffsetSeconds: new(-14400), UTCKey: "2026-03-11T03:40:00.000000000Z"}},
		{"example-3.json", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T23:30:00", TimezoneKind: "omitted"}},
		{"example-4.json", document.DocumentEventV1{Precision: "date", DateValue: "2026-03-10"}},
		{"example-5.json", document.DocumentEventV1{Precision: "date", DateValue: "2026-03-09"}},
		{"example-6.json", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T23:50:00", TimezoneKind: "utc", ZoneText: "UTC", UTCKey: "2026-03-10T23:50:00.000000000Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			tc.claim.EventID = "evt-55"
			tc.claim.DateKind = "imported"
			tc.claim.ClaimBasis = "source_asserted"
			raw, err := os.ReadFile(filepath.Join("testdata/when", tc.path))
			if err != nil {
				t.Fatal(err)
			}
			var want, got map[string]any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			emitted, err := json.Marshal(whenFromClaim(tc.claim))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(emitted, &got); err != nil {
				t.Fatal(err)
			}
			// The examples are zoned and some describe ranges or other providers.
			// This route emits single Docbank claims with its own provenance.
			for _, field := range []string{"day", "axis_minute", "end_at", "end_local", "end_date"} {
				delete(want, field)
			}
			want["placed_by"] = map[string]any{"source": string(tc.claim.DateKind), "rule": "primary-rule/v1", "fallback": true, "event_id": tc.claim.EventID, "claim_basis": tc.claim.ClaimBasis}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("got %s; want %v", emitted, want)
			}
		})
	}
}

// TestWhenFieldTableAllProducerCombinations checks field presence for reachable
// producer claims, including independently retained calendar instants.
func TestWhenFieldTableAllProducerCombinations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		claim  document.DocumentEventV1
		fields string
	}{
		{"date", document.DocumentEventV1{Precision: "date", DateValue: "2026-03-10", TimezoneKind: "omitted"}, "state precision placed_by date"},
		{"hour floating", document.DocumentEventV1{Precision: "hour", DateValue: "2026-03-10T09", TimezoneKind: "omitted"}, "state precision placed_by local zone_state"},
		{"minute email omitted", document.DocumentEventV1{Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "omitted", ZoneText: "-0000"}, "state precision placed_by local zone_state"},
		{"minute email named", document.DocumentEventV1{Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "named", ZoneText: "XYZ"}, "state precision placed_by local zone_state source_zone"},
		{"minute email RFC5322", document.DocumentEventV1{Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "rfc5322", ZoneText: "PST", OffsetSeconds: new(-28800), UTCKey: "2026-03-10T17:12:00.000000000Z"}, "state precision placed_by at local zone_state source_zone source_offset_seconds"},
		{"second UTC", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T09:12:30", TimezoneKind: "utc", ZoneText: "Z", UTCKey: "2026-03-10T09:12:30.000000000Z"}, "state precision placed_by at local zone_state source_zone source_offset_seconds"},
		{"second offset", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T09:12:30", TimezoneKind: "offset", ZoneText: "+01:00", OffsetSeconds: new(3600), UTCKey: "2026-03-10T08:12:30.000000000Z"}, "state precision placed_by at local zone_state source_zone source_offset_seconds"},
		{"second calendar IANA", document.DocumentEventV1{Precision: "second", DateValue: "2026-03-10T09:12:30", TimezoneKind: "iana", ZoneText: "America/Toronto", OffsetSeconds: new(-14400), UTCKey: "2026-03-10T13:12:30.000000000Z"}, "state precision placed_by at local zone_state source_zone source_offset_seconds"},
		{"fraction email RFC5322", document.DocumentEventV1{Precision: "fraction", FractionDigits: 3, DateValue: "2026-03-10T09:12:30.123", TimezoneKind: "rfc5322", ZoneText: "PST", OffsetSeconds: new(-28800), UTCKey: "2026-03-10T17:12:30.123000000Z"}, "state precision placed_by at local zone_state source_zone source_offset_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := versionDatesAPI(store.DocumentDateFacts{State: "published", View: store.DocumentEventView{Events: document.DocumentEventsV1{Events: []document.DocumentEventV1{tc.claim}}}}, "safe", "")
			raw, err := json.Marshal(out.Claims[0].When)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, field := range strings.Fields(tc.fields) {
				want[field] = true
			}
			if len(fields) != len(want) {
				t.Errorf("fields %s, want %v", raw, want)
			}
			for field := range fields {
				if !want[field] {
					t.Errorf("unexpected %s in %s", field, raw)
				}
			}
		})
	}
}
