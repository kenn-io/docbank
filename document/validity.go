package document

import (
	"errors"
	"fmt"
	"time"
)

// ValidityState describes whether a document is in force on a calendar date.
type ValidityState string

const (
	ValidityCurrent         ValidityState = "current"
	ValiditySuperseded      ValidityState = "superseded"
	ValidityExpired         ValidityState = "expired"
	ValidityNotYetEffective ValidityState = "not_yet_effective"
)

// EffectiveWindow is inclusive at both ends. An empty endpoint is open.
type EffectiveWindow struct{ From, To string }

// SuccessorStart is the effective start of a live successor.
type SuccessorStart struct {
	From string
}

// ValidateDate accepts a canonical calendar date, or an empty open endpoint.
func ValidateDate(date string) error {
	if date == "" {
		return nil
	}
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil || parsed.Format("2006-01-02") != date {
		return fmt.Errorf("invalid calendar date %q", date)
	}
	return nil
}

// ValidateEffectiveWindow validates canonical inclusive endpoints.
func ValidateEffectiveWindow(w EffectiveWindow) error {
	if err := ValidateDate(w.From); err != nil {
		return err
	}
	if err := ValidateDate(w.To); err != nil {
		return err
	}
	if w.From != "" && w.To != "" && w.From > w.To {
		return errors.New("effective window ends before it starts")
	}
	return nil
}

// EvaluateValidity applies precedence to a validated date and window. The
// boolean indicates an undated successor when supersession applies.
func EvaluateValidity(asOf string, own EffectiveWindow, successors []SuccessorStart) (ValidityState, bool) {
	if own.From != "" && own.From > asOf {
		return ValidityNotYetEffective, false
	}
	superseded, undated := false, false
	for _, successor := range successors {
		if successor.From == "" {
			superseded, undated = true, true
		} else if successor.From <= asOf {
			superseded = true
		}
	}
	if superseded {
		return ValiditySuperseded, undated
	}
	if own.To != "" && own.To < asOf {
		return ValidityExpired, false
	}
	return ValidityCurrent, false
}
