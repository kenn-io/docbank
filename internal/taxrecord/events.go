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
	RecordRevision int64
	Voided         bool
}

// ValidStateForKind reports whether the state belongs to the record kind.
func ValidStateForKind(kind, state string) bool {
	if _, err := InitialState(kind); err != nil {
		return false
	}
	if kind == "audit" || kind == "review" {
		return slices.Contains(auditStates, state)
	}
	return slices.Contains(filingStates, state)
}

// NormalizeEvent bounds and canonicalizes event input.
func NormalizeEvent(recordKind string, v EventValues) (EventValues, error) {
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

// EventState incrementally derives state from validated event facts.
type EventState struct {
	kind, state string
	latest      int64
}

func NewEventState(kind string) (EventState, error) {
	state, err := InitialState(kind)
	return EventState{kind: kind, state: state}, err
}

func (s *EventState) Apply(event EventFact) error {
	if err := ValidateEvent(s.kind, event.EventValues); err != nil {
		return err
	}
	if !event.Voided && event.SetsState != "" && event.RecordRevision > s.latest {
		s.latest, s.state = event.RecordRevision, event.SetsState
	}
	return nil
}

func (s *EventState) State() string { return s.state }

// StateFromEvents derives state from surviving events by append revision.
func StateFromEvents(recordKind string, events []EventFact) (string, error) {
	state, err := NewEventState(recordKind)
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if err := state.Apply(event); err != nil {
			return "", err
		}
	}
	return state.State(), nil
}

// NormalizeVoidReason validates a nonblank canonical reason for voiding.
func NormalizeVoidReason(reason string) (string, error) {
	if len(reason) > 8192 {
		return "", invalid("reason", "invalid length or text")
	}
	reason = note(reason)
	if err := text(reason, "reason", 8192, 0, true, true); err != nil {
		return "", err
	}
	return reason, nil
}
