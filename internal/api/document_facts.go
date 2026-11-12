package api

import (
	"slices"
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

func safeDocumentEvent(event document.DocumentEventV1) bool {
	return !event.Sensitive && !slices.ContainsFunc(event.Actors, func(actor document.DocumentEventActorV1) bool { return actor.Sensitive })
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
		w.State = "exact"
		if at, err := time.Parse(time.RFC3339Nano, event.UTCKey); err == nil {
			w.At = at.UTC().Format(time.RFC3339Nano)
		}
		w.SourceZone = event.ZoneText
		w.SourceOffsetSeconds = event.OffsetSeconds
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
		}
	}
	return w
}
