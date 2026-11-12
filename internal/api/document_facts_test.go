package api

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
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
		{"month", document.DocumentEventV1{Precision: "month", DateValue: "2026-03"}, `{"state":"coarse","precision":"month","period":"2026-03"}`},
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
	f := store.DocumentPeopleFacts{Version: store.ContentVersion{ID: "version", NodeID: 1}, State: "published", Persons: map[string]store.FactPerson{"visible": {Person: store.Person{PersonID: "visible", DisplayName: "Visible"}, Visible: true}, "hidden": {Person: store.Person{PersonID: "hidden", DisplayName: "Hidden"}}}, Edges: []document.DocumentPersonEdgeV1{{PersonID: "visible", Role: "author"}, {PersonID: "hidden", Role: "author"}, {PersonID: "visible", Role: "blind_copy", Sensitive: true}}, Assertions: []store.PersonDocumentAssertion{{PersonID: "hidden", Role: "author"}}, Candidates: []store.VersionCandidate{{CandidateID: "candidate", DisplayName: "Recipient", Occurrences: []store.PersonCandidateOccurrence{{Role: "blind_copy"}, {Role: "author"}}}}}
	f.Persons["visible"] = store.FactPerson{Person: store.Person{PersonID: "visible", DisplayName: "Private Bcc", State: "provisional", Origin: "derived"}, Visible: true}
	f.Edges[0].RawLabel = "Public Author"
	// The store supplies already-filtered candidate occurrences for safe reads.
	safeFacts := f
	safeFacts.Edges = f.Edges[:2]
	safeFacts.Candidates = []store.VersionCandidate{{CandidateID: "candidate", DisplayName: "Private Name", Occurrences: []store.PersonCandidateOccurrence{{Role: "author"}}, OccurrenceLabels: map[store.PersonCandidateOccurrence]string{{Role: "author"}: "Recipient"}}}
	safe := versionPeopleAPI(safeFacts, true, "")
	if len(safe.Edges) != 1 || safe.Edges[0].PersonID != "visible" || safe.Edges[0].DisplayName != "Public Author" || len(safe.Assertions) != 0 || len(safe.Unresolved) != 1 || safe.Unresolved[0].Role != "author" || safe.Unresolved[0].RawLabel != "Recipient" {
		t.Fatalf("safe: %+v", safe)
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

// TestWhenFieldTableAllProducerCombinations checks the shared contract's field
// table independently of the example fixtures for every supported precision and zone.
func TestWhenFieldTableAllProducerCombinations(t *testing.T) {
	t.Parallel()
	values := map[document.EventPrecision]string{"year": "2026", "month": "2026-03", "date": "2026-03-10", "hour": "2026-03-10T09", "minute": "2026-03-10T09:12", "second": "2026-03-10T09:12:30", "fraction": "2026-03-10T09:12:30.123"}
	for _, precision := range document.AllEventPrecisions() {
		for _, zone := range document.AllEventTimezoneKinds() {
			for _, zoneText := range []string{"", "source zone"} {
				claim := document.DocumentEventV1{Precision: precision, DateValue: values[precision], TimezoneKind: zone, ZoneText: zoneText}
				if zone == "offset" {
					claim.OffsetSeconds = new(3600)
				}
				key, _, err := document.EventUTCKey(claim.DateValue, precision, zone, claim.OffsetSeconds)
				if err != nil {
					t.Fatal(err)
				}
				claim.UTCKey = key
				out := versionDatesAPI(store.DocumentDateFacts{State: "published", View: store.DocumentEventView{Events: document.DocumentEventsV1{Events: []document.DocumentEventV1{claim}}}}, "safe", "")
				raw, err := json.Marshal(out.Claims[0].When)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				want := map[string]bool{"state": true, "precision": true, "placed_by": true}
				switch precision {
				case "year", "month":
					want["period"] = true
				case "date":
					want["date"] = true
				default:
					want["zone_state"] = true
					if key != "" {
						want["at"] = true
						if zone != "offset" || zoneText != "" {
							want["local"], want["source_zone"], want["source_offset_seconds"] = true, true, true
						}
					} else {
						want["local"] = true
						if zone != "omitted" && zoneText != "" {
							want["source_zone"] = true
						}
					}
				}
				if len(fields) != len(want) {
					t.Errorf("%s/%s/%q: fields %s, want %v", precision, zone, zoneText, raw, want)
					continue
				}
				for field := range fields {
					if !want[field] {
						t.Errorf("%s/%s/%q: unexpected %s in %s", precision, zone, zoneText, field, raw)
					}
				}
			}
		}
	}
}
