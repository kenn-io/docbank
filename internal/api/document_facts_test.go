package api

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestSafeDocumentEventVisibility(t *testing.T) {
	tests := []struct {
		name    string
		event   document.DocumentEventV1
		visible bool
	}{
		{name: "ordinary", visible: true},
		{name: "sensitive event", event: document.DocumentEventV1{Sensitive: true}},
		{name: "mixed recipients including bcc", event: document.DocumentEventV1{Actors: []document.DocumentEventActorV1{{Role: "sender"}, {Role: "blind_copy", Sensitive: true}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeDocumentEvent(tt.event); got != tt.visible {
				t.Errorf("visibility = %v, want %v", got, tt.visible)
			}
		})
	}
}

func TestWhenFromClaimNormalizesValues(t *testing.T) {
	tests := []struct {
		name                   string
		claim                  document.DocumentEventV1
		state, local, at, zone string
		offset                 *int
	}{
		{name: "minute offset", claim: document.DocumentEventV1{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "offset", ZoneText: "-04:00", UTCKey: "2026-03-10T13:12:00.000000000Z", OffsetSeconds: new(-14400)}, state: "exact", local: "2026-03-10T09:12:00", at: "2026-03-10T13:12:00Z", zone: "offset", offset: new(-14400)},
		{name: "utc", claim: document.DocumentEventV1{DateKind: "created", Precision: "hour", DateValue: "2026-03-10T09", TimezoneKind: "utc", UTCKey: "2026-03-10T09:00:00.000000000Z"}, state: "exact", local: "2026-03-10T09:00:00", at: "2026-03-10T09:00:00Z", zone: "utc", offset: new(0)},
		{name: "omitted", claim: document.DocumentEventV1{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "omitted"}, state: "floating", local: "2026-03-10T09:12:00", zone: "omitted"},
		{name: "date", claim: document.DocumentEventV1{DateKind: "created", Precision: "date", DateValue: "2026-03-10"}, state: "date"},
		{name: "month", claim: document.DocumentEventV1{DateKind: "created", Precision: "month", DateValue: "2026-03"}, state: "coarse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := whenFromClaim(tt.claim)
			if w.State != tt.state || w.Local != tt.local || w.At != tt.at || w.ZoneState != tt.zone {
				t.Errorf("when = %+v", w)
			}
			if (w.SourceOffsetSeconds == nil) != (tt.offset == nil) || tt.offset != nil && *w.SourceOffsetSeconds != *tt.offset {
				t.Errorf("offset = %v, want %v", w.SourceOffsetSeconds, tt.offset)
			}
			if w.State == "date" && w.Date != tt.claim.DateValue || w.State == "coarse" && w.Period != tt.claim.DateValue {
				t.Errorf("calendar value = %+v", w)
			}
		})
	}
}

func TestSafePeopleFactsFiltersBlindCopies(t *testing.T) {
	facts := VersionPeople{Edges: []VersionPersonEdge{{Role: "blind_copy"}, {Role: "author"}}, Assertions: []VersionPersonAssertion{{Role: "blind_copy"}, {Role: "author"}}, Unresolved: []VersionUnresolvedName{{Role: "blind_copy"}, {Role: "author"}}}
	filterSafePeople(&facts)
	if len(facts.Edges) != 1 || facts.Edges[0].Role != "author" || len(facts.Assertions) != 1 || facts.Assertions[0].Role != "author" || len(facts.Unresolved) != 1 || facts.Unresolved[0].Role != "author" {
		t.Fatalf("unsafe facts: %+v", facts)
	}
}

func TestWhenFieldTable(t *testing.T) {
	claims := []document.DocumentEventV1{
		{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "offset", ZoneText: "-04:00", UTCKey: "2026-03-10T13:12:00.000000000Z", OffsetSeconds: new(-14400)},
		{DateKind: "created", Precision: "second", DateValue: "2026-03-10T09:12:00", TimezoneKind: "utc", UTCKey: "2026-03-10T09:12:00.000000000Z"},
		{DateKind: "sent", Precision: "minute", DateValue: "2026-03-10T09:12", TimezoneKind: "omitted", ZoneText: "-0000"},
		{DateKind: "created", Precision: "date", DateValue: "2026-03-10"},
		{DateKind: "created", Precision: "month", DateValue: "2026-03"},
	}
	response := versionDatesAPI(store.DocumentDateFacts{Version: store.ContentVersion{ID: "version", NodeID: 1}, State: "published", View: store.DocumentEventView{Events: document.DocumentEventsV1{Events: claims}}}, "safe", "")
	for _, returned := range response.Claims {
		w := returned.When
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if w.State != "exact" {
			if _, ok := fields["at"]; ok {
				t.Errorf("non-exact instant on wire: %s", raw)
			}
		}
		if w.State == "date" || w.State == "coarse" {
			if _, ok := fields["local"]; ok {
				t.Errorf("civil date has local time: %s", raw)
			}
			if _, ok := fields["zone_state"]; ok {
				t.Errorf("civil date has zone: %s", raw)
			}
		}
		if w.ZoneState == "omitted" {
			if _, ok := fields["source_zone"]; ok {
				t.Errorf("omitted zone leaked text: %s", raw)
			}
		}
		switch w.State {
		case "exact":
			if w.At == "" || w.Local == "" || w.SourceZone == "" || w.SourceOffsetSeconds == nil || w.Date != "" || w.Period != "" {
				t.Errorf("invalid exact: %+v", w)
			}
		case "floating":
			if w.At != "" || w.Local == "" || w.SourceOffsetSeconds != nil || w.Date != "" || w.Period != "" || w.ZoneState == "omitted" && w.SourceZone != "" {
				t.Errorf("invalid floating: %+v", w)
			}
		case "date":
			if w.At != "" || w.Local != "" || w.ZoneState != "" || w.Date == "" || w.Period != "" {
				t.Errorf("invalid date: %+v", w)
			}
		case "coarse":
			if w.At != "" || w.Local != "" || w.ZoneState != "" || w.Date != "" || w.Period == "" {
				t.Errorf("invalid coarse: %+v", w)
			}
		default:
			t.Errorf("unknown state: %+v", w)
		}
	}
}

func TestWhenContractExamplesDecode(t *testing.T) {
	paths, err := filepath.Glob("testdata/when/*.json")
	if err != nil || len(paths) != 6 {
		t.Fatalf("fixtures: %v %v", paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var w WhenV1
			if err := json.Unmarshal(raw, &w); err != nil {
				t.Fatal(err)
			}
			if w.State == "" || w.Precision == "" || w.PlacedBy.Rule == "" {
				t.Fatalf("missing contract fields: %+v", w)
			}
		})
	}
}

func TestWhenMissingSourceZoneRemainsContractValid(t *testing.T) {
	exact := whenFromClaim(document.DocumentEventV1{DateKind: "created", Precision: "second", DateValue: "2026-03-10T09:12:00", TimezoneKind: "offset", OffsetSeconds: new(3600), UTCKey: "2026-03-10T08:12:00.000000000Z"})
	if exact.ZoneState != "unknown" || exact.At == "" || exact.Local != "" || exact.SourceZone != "" || exact.SourceOffsetSeconds != nil {
		t.Fatalf("unknown source zone: %+v", exact)
	}
	floating := whenFromClaim(document.DocumentEventV1{DateKind: "created", Precision: "second", DateValue: "2026-03-10T09:12:00", TimezoneKind: "named"})
	if floating.ZoneState != "omitted" || floating.SourceZone != "" {
		t.Fatalf("missing zone: %+v", floating)
	}
}

func TestVersionDatesProjectionUsesSelectedDisclosure(t *testing.T) {
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
	f := store.DocumentPeopleFacts{Version: store.ContentVersion{ID: "version", NodeID: 1}, State: "published", Persons: map[string]store.FactPerson{"visible": {Person: store.Person{DisplayName: "Visible"}, Visible: true}, "hidden": {Person: store.Person{DisplayName: "Hidden"}}}, Edges: []document.DocumentPersonEdgeV1{{PersonID: "visible", Role: "author"}, {PersonID: "hidden", Role: "author"}, {PersonID: "visible", Role: "blind_copy", Sensitive: true}}, Assertions: []store.PersonDocumentAssertion{{PersonID: "visible", Role: "blind_copy"}, {PersonID: "hidden", Role: "author"}}, Candidates: []store.VersionCandidate{{CandidateID: "candidate", DisplayName: "Recipient", Occurrences: []store.PersonCandidateOccurrence{{Role: "blind_copy"}, {Role: "author"}}}}}
	safe := versionPeopleAPI(f, true, "")
	if len(safe.Edges) != 1 || safe.Edges[0].PersonID != "visible" || len(safe.Assertions) != 0 || len(safe.Unresolved) != 1 || safe.Unresolved[0].Role != "author" {
		t.Fatalf("safe: %+v", safe)
	}
	full := versionPeopleAPI(f, false, "")
	if len(full.Edges) != 3 || len(full.Assertions) != 2 || len(full.Unresolved) != 2 {
		t.Fatalf("full: %+v", full)
	}
}
