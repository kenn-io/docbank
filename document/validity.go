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
	if _, err := eventCivil(date, "date"); err != nil {
		return fmt.Errorf("invalid calendar date %q", date)
	}
	return nil
}

// ResolveValidityDate defaults an omitted date to today and validates it.
func ResolveValidityDate(asOf string) (string, error) {
	if asOf == "" {
		asOf = time.Now().Format("2006-01-02")
	}
	if err := ValidateDate(asOf); err != nil {
		return "", err
	}
	return asOf, nil
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

// SuccessorApplies reports whether a successor takes effect on a validated date
// and window. An undated successor waits until the node's inclusive end passes.
func SuccessorApplies(asOf string, own EffectiveWindow, successor SuccessorStart) bool {
	if successor.From == "" {
		return own.To == "" || own.To < asOf
	}
	return successor.From <= asOf
}

// EvaluateValidity applies precedence to a validated date and window. The
// boolean indicates an undated successor when supersession applies.
func EvaluateValidity(asOf string, own EffectiveWindow, successors []SuccessorStart) (ValidityState, bool) {
	if own.From != "" && own.From > asOf {
		return ValidityNotYetEffective, false
	}
	superseded, undated := false, false
	for _, successor := range successors {
		if SuccessorApplies(asOf, own, successor) {
			superseded = true
			undated = undated || successor.From == ""
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
