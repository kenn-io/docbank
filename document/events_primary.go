package document

import (
	"cmp"
	"slices"
)

// PrimaryRuleV1 identifies the vault primary-date policy.
const PrimaryRuleV1 = "primary-rule/v1"

var primaryRuleOrders = map[DocumentKind][]DateKind{
	"email":       {"sent", "received", "created", "modified"},
	"message":     {"sent", "received", "authored", "created", "modified"},
	"calendar":    {"started", "ended", "authored", "created", "modified"},
	"image":       {"captured", "created", "authored", "modified"},
	"audio_video": {"captured", "authored", "created", "modified", "produced"},
	"other":       {"authored", "created", "modified", "produced"},
}

var primaryReasons = map[DateKind]string{
	"sent":           "email_date_header",
	"received":       "received_date",
	"document_date":  "supplied_document_date",
	"authored":       "authored_date",
	"created":        "created_date",
	"modified":       "modified_date",
	"captured":       "captured_date",
	"started":        "started_date",
	"ended":          "ended_date",
	"produced":       "produced_date",
	"imported":       "imported_fallback",
	"vault_recorded": "docbank_recorded_fallback",
}

// SelectPrimaryEvent selects an explainable date from already validated events.
// Safe disclosure excludes events with sensitive dates or attached actors.
// It does not modify the supplied events.
func SelectPrimaryEvent(events []DocumentEventV1, kind DocumentKind, scopeClass, disclosure string) (DocumentEventPrimaryV1, bool) {
	if scopeClass != "vault" || (disclosure != "safe" && disclosure != "full") || !ValidDocumentKind(kind) {
		return DocumentEventPrimaryV1{}, false
	}
	order := make([]DateKind, 0, len(primaryRuleOrders[kind])+3)
	order = append(order, primaryRuleOrders[kind]...)
	order = append(order, "document_date", "imported", "vault_recorded")
	for _, dateKind := range order {
		var best *DocumentEventV1
		for i := range events {
			event := &events[i]
			if event.DateKind != dateKind || (disclosure == "safe" && eventHasSensitiveEvidence(*event)) {
				continue
			}
			if best == nil || comparePrimaryEvents(*event, *best) < 0 {
				best = event
			}
		}
		if best != nil {
			return DocumentEventPrimaryV1{EventID: best.EventID, Reason: primaryReasons[dateKind], RuleID: PrimaryRuleV1, ScopeClass: scopeClass, Disclosure: disclosure}, true
		}
	}
	return DocumentEventPrimaryV1{}, false
}

func eventHasSensitiveEvidence(event DocumentEventV1) bool {
	return event.Sensitive || slices.ContainsFunc(event.Actors, func(actor DocumentEventActorV1) bool {
		return actor.Sensitive
	})
}

func primaryReliability(e DocumentEventV1) int {
	if e.ClaimBasis == "docbank_observed" {
		return 1
	}
	return 0
}

func comparePrimaryEvents(a, b DocumentEventV1) int {
	if c := cmp.Compare(primaryReliability(a), primaryReliability(b)); c != 0 {
		return c
	}
	if (a.ParseConfidence == "profile_interpreted") != (b.ParseConfidence == "profile_interpreted") {
		if a.ParseConfidence == "profile_interpreted" {
			return 1
		}
		return -1
	}
	if (a.UTCKey != "") != (b.UTCKey != "") {
		if a.UTCKey != "" {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(slices.Index(eventPrecisions, b.Precision), slices.Index(eventPrecisions, a.Precision)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.SourceKey, b.SourceKey); c != 0 {
		return c
	}
	return cmp.Compare(a.EventID, b.EventID)
}
