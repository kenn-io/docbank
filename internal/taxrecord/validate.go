// Package taxrecord validates tax authority independent of its storage.
package taxrecord

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/currency"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// ErrInvalid identifies invalid tax input without exposing its value.
var ErrInvalid = errors.New("invalid tax value")

// FieldError identifies the invalid field without retaining its value.
type FieldError struct{ Field, Reason string }

func (e *FieldError) Error() string      { return fmt.Sprintf("%s: %s", e.Field, e.Reason) }
func (e *FieldError) Unwrap() error      { return ErrInvalid }
func invalid(field, reason string) error { return &FieldError{field, reason} }

// AccountValues contains the canonical account fields.
type AccountValues struct {
	FilerPartyID                                                            string
	AuthorityPartyID                                                        *string
	Country, Subdivision, TaxKind, Identifier, Cadence, PeriodNote, DueNote string
	LeadDays                                                                int64
	Currency                                                                string
	EffectiveStart, EffectiveEnd                                            *string
	Note                                                                    string
}

// RecordValues contains the canonical record fields.
type RecordValues struct {
	AccountID                                            *string
	FilerPartyID                                         string
	AuthorityPartyID                                     *string
	Kind, Title, Country, Subdivision, TaxKind, FormCode string
	TaxYear                                              *int64
	PeriodStart, PeriodEnd, DueDate                      *string
	LeadDays                                             *int64
	State, Direction                                     string
	AmountMinor                                          *int64
	Currency, Verification                               string
	VerifiedOn                                           *string
	VerifiedRevision                                     *int64
	VerificationNote, Note                               string
}

func text(s, field string, bytes, runes int, required, note bool) error {
	if len(s) > bytes || !utf8.ValidString(s) || (runes > 0 && utf8.RuneCountInString(s) > runes) || (required && strings.TrimSpace(s) == "") {
		return invalid(field, "invalid length or text")
	}
	for _, c := range s {
		if unicode.IsControl(c) && !(note && (c == '\t' || c == '\n')) {
			return invalid(field, "control character")
		}
	}
	return nil
}
func label(s string) string { return norm.NFC.String(strings.TrimSpace(s)) }
func note(s string) string  { return strings.ReplaceAll(s, "\r\n", "\n") }
func rawLocation(country, subdivision, code string) error {
	for _, item := range []struct {
		field, value string
		limit        int
	}{{"country", country, 2}, {"subdivision", subdivision, 6}, {"currency", code, 3}} {
		if len(item.value) > item.limit || !utf8.ValidString(item.value) {
			return invalid(item.field, "invalid length or text")
		}
	}
	return nil
}
func location(country, subdivision, code string, requireCurrency bool) error {
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return invalid("country", "invalid country")
	}
	region, err := language.ParseRegion(country)
	if err != nil || region.String() != country || (!region.IsCountry() && country != "ZZ") {
		return invalid("country", "invalid country")
	}
	if subdivision != "" {
		if len(subdivision) < 4 || len(subdivision) > 6 || !strings.HasPrefix(subdivision, country+"-") {
			return invalid("subdivision", "invalid subdivision")
		}
		for _, c := range subdivision[3:] {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return invalid("subdivision", "invalid subdivision")
			}
		}
	}
	if code == "" && !requireCurrency {
		return nil
	}
	if len(code) != 3 || code != strings.ToUpper(code) {
		return invalid("currency", "invalid currency")
	}
	if _, err := currency.ParseISO(code); err != nil {
		return invalid("currency", "invalid currency")
	}
	return nil
}
func date(field string, d *string) error {
	if d == nil {
		return nil
	}
	if len(*d) != 10 || (*d)[:4] == "0000" {
		return invalid(field, "invalid date")
	}
	v, err := time.Parse("2006-01-02", *d)
	if err != nil || v.Format("2006-01-02") != *d {
		return invalid(field, "invalid date")
	}
	return nil
}
func dates(startName, endName string, start, end *string) error {
	if err := date(startName, start); err != nil {
		return err
	}
	if err := date(endName, end); err != nil {
		return err
	}
	if start != nil && end != nil && *start > *end {
		return invalid(endName, "precedes "+startName)
	}
	return nil
}

// InitialState returns the only state accepted on creation.
func InitialState(kind string) (string, error) {
	switch kind {
	case "filing", "payment", "penalty", "notice", "other":
		return "expected", nil
	case "audit", "review":
		return "opened", nil
	default:
		return "", invalid("kind", "unknown kind")
	}
}

// ValidState reports whether state is in the tax record vocabulary.
func ValidState(state string) bool {
	switch state {
	case "expected", "gathering", "ready", "filed", "assessed", "settled", "amended", "disputed", "not_required", "opened", "information_requested", "responding", "decision_issued", "appealed", "closed":
		return true
	default:
		return false
	}
}

// NormalizeAccount canonicalizes and validates account input.
func NormalizeAccount(v AccountValues) (AccountValues, error) {
	if err := rawLocation(v.Country, v.Subdivision, v.Currency); err != nil {
		return v, err
	}
	// Bounds precede normalization and parsing.
	if err := text(v.Identifier, "identifier", 256, 0, false, false); err != nil {
		return v, err
	}
	for _, item := range []struct{ field, value string }{{"period_note", v.PeriodNote}, {"due_note", v.DueNote}, {"note", v.Note}} {
		if len(item.value) > 8192 || !utf8.ValidString(item.value) {
			return v, invalid(item.field, "invalid length or text")
		}
	}
	if err := text(v.TaxKind, "tax_kind", 256, 0, true, false); err != nil {
		return v, err
	}
	v.TaxKind = label(v.TaxKind)
	v.Country = strings.ToUpper(v.Country)
	v.Subdivision = strings.ToUpper(v.Subdivision)
	v.Currency = strings.ToUpper(v.Currency)
	v.PeriodNote, v.DueNote, v.Note = note(v.PeriodNote), note(v.DueNote), note(v.Note)
	return v, ValidateAccount(v)
}

// ValidateAccount checks canonical retained account values.
func ValidateAccount(v AccountValues) error {
	if err := rawLocation(v.Country, v.Subdivision, v.Currency); err != nil {
		return err
	}
	if v.FilerPartyID == "" {
		return invalid("filer_party_id", "required")
	}
	if err := text(v.TaxKind, "tax_kind", 256, 64, true, false); err != nil {
		return err
	}
	if label(v.TaxKind) != v.TaxKind {
		return invalid("tax_kind", "not canonical")
	}
	for _, item := range []struct {
		field, value string
		limit        int
		note         bool
	}{{"identifier", v.Identifier, 256, false}, {"period_note", v.PeriodNote, 8192, true}, {"due_note", v.DueNote, 8192, true}, {"note", v.Note, 8192, true}} {
		if err := text(item.value, item.field, item.limit, 0, false, item.note); err != nil {
			return err
		}
	}
	if err := location(v.Country, v.Subdivision, v.Currency, true); err != nil {
		return err
	}
	switch v.Cadence {
	case "monthly", "bimonthly", "quarterly", "semiannual", "annual", "event", "other":
	default:
		return invalid("cadence", "unknown cadence")
	}
	if v.LeadDays < 0 || v.LeadDays > 366 {
		return invalid("lead_days", "out of range")
	}
	return dates("effective_start", "effective_end", v.EffectiveStart, v.EffectiveEnd)
}

// NormalizeRecord canonicalizes and validates record input.
func NormalizeRecord(v RecordValues) (RecordValues, error) {
	if err := rawLocation(v.Country, v.Subdivision, v.Currency); err != nil {
		return v, err
	}
	for _, item := range []struct{ field, value string }{{"note", v.Note}, {"verification_note", v.VerificationNote}} {
		if len(item.value) > 8192 || !utf8.ValidString(item.value) {
			return v, invalid(item.field, "invalid length or text")
		}
	}
	for _, item := range []struct {
		field, value string
		limit        int
	}{{"title", v.Title, 1200}, {"tax_kind", v.TaxKind, 256}, {"form_code", v.FormCode, 256}} {
		if err := text(item.value, item.field, item.limit, 0, false, false); err != nil {
			return v, err
		}
	}
	v.Title, v.TaxKind, v.FormCode = label(v.Title), label(v.TaxKind), label(v.FormCode)
	v.Country, v.Subdivision, v.Currency = strings.ToUpper(v.Country), strings.ToUpper(v.Subdivision), strings.ToUpper(v.Currency)
	v.Note, v.VerificationNote = note(v.Note), note(v.VerificationNote)
	return v, ValidateRecord(v)
}

// ValidateRecord checks canonical retained record values supported by this store.
func ValidateRecord(v RecordValues) error {
	if err := rawLocation(v.Country, v.Subdivision, v.Currency); err != nil {
		return err
	}
	if v.FilerPartyID == "" {
		return invalid("filer_party_id", "required")
	}
	for _, item := range []struct {
		field, value string
		bytes, runes int
		required     bool
	}{{"title", v.Title, 1200, 300, true}, {"tax_kind", v.TaxKind, 256, 64, true}, {"form_code", v.FormCode, 256, 64, false}} {
		if err := text(item.value, item.field, item.bytes, item.runes, item.required, false); err != nil {
			return err
		}
		if label(item.value) != item.value {
			return invalid(item.field, "not canonical")
		}
	}
	for _, item := range []struct{ field, value string }{{"note", v.Note}, {"verification_note", v.VerificationNote}} {
		if err := text(item.value, item.field, 8192, 0, false, true); err != nil {
			return err
		}
	}
	if err := location(v.Country, v.Subdivision, v.Currency, v.AmountMinor != nil); err != nil {
		return err
	}
	state, err := InitialState(v.Kind)
	if err != nil {
		return err
	}
	if v.State != state {
		return invalid("state", "unsupported state")
	}
	if v.Verification != "unverified" || v.VerifiedOn != nil || v.VerifiedRevision != nil || v.VerificationNote != "" {
		return invalid("verification", "unsupported verification")
	}
	if err := dates("period_start", "period_end", v.PeriodStart, v.PeriodEnd); err != nil {
		return err
	}
	if err := date("due_date", v.DueDate); err != nil {
		return err
	}
	if v.TaxYear != nil && (*v.TaxYear < 1 || *v.TaxYear > 9999) {
		return invalid("tax_year", "out of range")
	}
	if v.LeadDays != nil && (*v.LeadDays < 0 || *v.LeadDays > 366) {
		return invalid("lead_days", "out of range")
	}
	switch v.Direction {
	case "unknown":
		if v.AmountMinor != nil {
			return invalid("amount", "unknown amount must be null")
		}
	case "payable", "refundable":
		if v.AmountMinor == nil || *v.AmountMinor < 0 {
			return invalid("amount", "amount required")
		}
	case "nil":
		if v.AmountMinor == nil || *v.AmountMinor != 0 {
			return invalid("amount", "zero required")
		}
	default:
		return invalid("direction", "unknown direction")
	}
	return nil
}
