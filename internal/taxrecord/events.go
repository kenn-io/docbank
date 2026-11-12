package taxrecord

import "slices"

// EventValues is the canonical, retained content of a dated event.
type EventValues struct {
	Kind, OccurredOn, SetsState, Currency, Reference, Note string
	AmountMinor                                            *int64
}

// EventFact supplies append order and void status for state derivation.
type EventFact struct {
	EventValues
	EventID        string
	RecordRevision int64
	Voided         bool
}

// ValidStateForKind reports whether the state belongs to the record kind.
func ValidStateForKind(kind, state string) bool {
	if _, err := InitialState(kind); err != nil {
		return false
	}
	if kind == "audit" || kind == "review" {
		return slices.Contains(recordStates[9:], state)
	}
	return slices.Contains(recordStates[:9], state)
}

// NormalizeEvent bounds and canonicalizes event input.
func NormalizeEvent(recordKind string, v EventValues) (EventValues, error) {
	if err := text(v.Reference, "reference", 256, 0, false, false); err != nil {
		return v, err
	}
	if len(v.Note) > 8192 {
		return v, invalid("note", "invalid length or text")
	}
	code, err := NormalizeCurrency(v.Currency)
	if err != nil {
		return v, err
	}
	v.Currency, v.Note = code, note(v.Note)
	return v, ValidateEvent(recordKind, v)
}

// ValidateEvent validates canonical authority accepted at this stage.
func ValidateEvent(recordKind string, v EventValues) error {
	if _, err := InitialState(recordKind); err != nil {
		return err
	}
	if v.Kind == "note" {
		if v.SetsState != "" {
			return invalid("sets_state", "invalid state")
		}
	} else if !ValidStateForKind(recordKind, v.Kind) || v.SetsState != v.Kind {
		return invalid("kind", "unsupported event")
	}
	if err := date("occurred_on", &v.OccurredOn); err != nil {
		return err
	}
	if err := text(v.Reference, "reference", 256, 0, false, false); err != nil {
		return err
	}
	if err := text(v.Note, "note", 8192, 0, false, true); err != nil {
		return err
	}
	if note(v.Note) != v.Note {
		return invalid("note", "not canonical")
	}
	if v.AmountMinor == nil {
		if v.Currency != "" {
			return invalid("currency", "requires amount")
		}
	} else {
		if *v.AmountMinor < 0 || (v.Kind != "assessed" && v.Kind != "decision_issued") {
			return invalid("amount", "unsupported amount")
		}
		if err := location("ZZ", "", v.Currency, true); err != nil {
			return err
		}
	}
	return nil
}

// StateFromEvents derives state from surviving events by append revision.
func StateFromEvents(recordKind string, events []EventFact) (string, error) {
	state, err := InitialState(recordKind)
	if err != nil {
		return "", err
	}
	var latest int64
	for _, event := range events {
		if err := ValidateEvent(recordKind, event.EventValues); err != nil {
			return "", err
		}
		if !event.Voided && event.SetsState != "" && event.RecordRevision > latest {
			latest, state = event.RecordRevision, event.SetsState
		}
	}
	return state, nil
}
