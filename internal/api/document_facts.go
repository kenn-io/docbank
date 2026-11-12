package api

import (
	"time"

	"go.kenn.io/docbank/document"
)

// WhenV1 describes the source's time without inventing a timezone.
type WhenV1 struct {
	State               string                  `json:"state"`
	Precision           document.EventPrecision `json:"precision"`
	At                  string                  `json:"at,omitempty"`
	Local               string                  `json:"local,omitempty"`
	ZoneState           string                  `json:"zone_state,omitempty"`
	SourceZone          string                  `json:"source_zone,omitempty"`
	SourceOffsetSeconds *int                    `json:"source_offset_seconds,omitempty"`
	Date                string                  `json:"date,omitempty"`
	Period              string                  `json:"period,omitempty"`
	PlacedBy            WhenPlacedBy            `json:"placed_by"`
}

type WhenPlacedBy struct {
	Source     document.DateKind `json:"source"`
	Rule       string            `json:"rule"`
	Fallback   bool              `json:"fallback"`
	EventID    string            `json:"event_id"`
	ClaimBasis string            `json:"claim_basis"`
}

func whenFromClaim(event document.DocumentEventV1) WhenV1 {
	w := WhenV1{Precision: event.Precision, PlacedBy: WhenPlacedBy{Source: event.DateKind, Rule: document.PrimaryRuleV1, Fallback: event.DateKind == "imported" || event.DateKind == "vault_recorded", EventID: event.EventID, ClaimBasis: event.ClaimBasis}}
	switch event.Precision {
	case "year", "month":
		w.State, w.Period = "coarse", event.DateValue
		return w
	case "date":
		w.State, w.Date = "date", event.DateValue
		return w
	}
	w.Local = event.DateValue
	switch event.Precision {
	case "hour":
		w.Local += ":00:00"
	case "minute":
		w.Local += ":00"
	}
	w.ZoneState = string(event.TimezoneKind)
	if event.UTCKey != "" {
		// Stored events are decoded and validated before projection; UTCKey is
		// recomputed by DecodeDocumentEventsV1 from its civil value.
		w.State = "exact"
		at, _ := time.Parse(time.RFC3339Nano, event.UTCKey)
		w.At = at.UTC().Format(time.RFC3339Nano)
		w.SourceZone = event.ZoneText
		w.SourceOffsetSeconds = event.OffsetSeconds
		if event.TimezoneKind == "offset" && w.SourceZone == "" {
			w.ZoneState = "unknown"
			w.Local = ""
			w.SourceOffsetSeconds = nil
		}
		if event.TimezoneKind == "utc" {
			if w.SourceZone == "" {
				w.SourceZone = "UTC"
			}
			w.SourceOffsetSeconds = new(0)
		}
	} else {
		w.State = "floating"
		if event.TimezoneKind != "omitted" {
			w.SourceZone = event.ZoneText
			if w.SourceZone == "" {
				w.ZoneState = "omitted"
			}
		}
	}
	return w
}

type FactOpen struct {
	Web string `json:"web"`
}
type VersionDateClaim struct {
	EventID         string                     `json:"event_id"`
	DateKind        document.DateKind          `json:"date_kind"`
	Primary         bool                       `json:"primary"`
	DateValue       string                     `json:"date_value"`
	RawValue        string                     `json:"raw_value"`
	Precision       document.EventPrecision    `json:"precision"`
	TimezoneKind    document.EventTimezoneKind `json:"timezone_kind"`
	ZoneText        string                     `json:"zone_text"`
	OffsetSeconds   *int                       `json:"offset_seconds,omitempty"`
	AxisKey         string                     `json:"axis_key"`
	UTCKey          string                     `json:"utc_key,omitempty"`
	ClaimBasis      string                     `json:"claim_basis"`
	ParseConfidence string                     `json:"parse_confidence"`
	EvidenceKind    document.EventEvidenceKind `json:"evidence_kind"`
	EvidenceID      string                     `json:"evidence_id"`
	SourceKindRaw   string                     `json:"source_kind_raw,omitempty"`
	When            WhenV1                     `json:"when"`
}
type FactDiagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}
type VersionDatePrimary struct {
	EventID    string `json:"event_id"`
	Reason     string `json:"reason"`
	RuleID     string `json:"rule_id"`
	Disclosure string `json:"disclosure"`
}
type VersionDates struct {
	ContentVersionID string                `json:"content_version_id"`
	NodeID           int64                 `json:"node_id"`
	State            string                `json:"state"`
	DocumentKind     document.DocumentKind `json:"document_kind,omitempty"`
	Primary          *VersionDatePrimary   `json:"primary,omitempty"`
	Claims           []VersionDateClaim    `json:"claims"`
	Diagnostics      []FactDiagnostic      `json:"diagnostics"`
	Open             *FactOpen             `json:"open,omitempty"`
}
type FactExternal struct {
	System    string `json:"system"`
	ArchiveID string `json:"archive_id"`
	UID       string `json:"uid"`
}
type VersionPersonEdge struct {
	PersonID      string         `json:"person_id"`
	DisplayName   string         `json:"display_name"`
	Role          string         `json:"role"`
	Basis         string         `json:"basis"`
	Confidence    string         `json:"confidence"`
	EvidenceKind  string         `json:"evidence_kind"`
	EvidenceID    string         `json:"evidence_id"`
	RawLabel      string         `json:"raw_label"`
	ClaimCount    int            `json:"claim_count"`
	LinkedContact bool           `json:"linked_contact"`
	External      []FactExternal `json:"external"`
}
type VersionPersonAssertion struct {
	AssertionID string `json:"assertion_id"`
	PersonID    string `json:"person_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Action      string `json:"action"`
	Note        string `json:"note"`
	RecordedAt  string `json:"recorded_at"`
	Revision    int64  `json:"revision"`
}
type VersionUnresolvedName struct {
	RawLabel     string `json:"raw_label"`
	Role         string `json:"role"`
	CandidateID  string `json:"candidate_id"`
	EvidenceKind string `json:"evidence_kind"`
}
type VersionPeople struct {
	ContentVersionID string                   `json:"content_version_id"`
	State            string                   `json:"state"`
	Edges            []VersionPersonEdge      `json:"edges"`
	Assertions       []VersionPersonAssertion `json:"assertions"`
	Unresolved       []VersionUnresolvedName  `json:"unresolved"`
	Open             *FactOpen                `json:"open,omitempty"`
}
