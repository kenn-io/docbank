// Package taxrecord validates tax authority independent of its storage.
package taxrecord

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"

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
	if !note {
		if !document.ValidPersonIdentityText(s) {
			return invalid(field, "control character")
		}
		return nil
	}
	for _, c := range s {
		if unicode.IsControl(c) && c != '\t' && c != '\n' {
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
		for i := range len(item.value) {
			if item.value[i] >= utf8.RuneSelf {
				return invalid(item.field, "invalid length or text")
			}
		}
	}
	return nil
}

// NormalizeCurrency bounds and canonicalizes raw currency text.
func NormalizeCurrency(code string) (string, error) {
	if err := rawLocation("", "", code); err != nil {
		return "", err
	}
	return strings.ToUpper(code), nil
}

// ValidateCountry checks a canonical tax country code.
func ValidateCountry(country string) error {
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return invalid("country", "invalid country")
	}
	region, err := language.ParseRegion(country)
	// x/text omits UN and EZ from its group table, though both are groups.
	if err != nil || region.String() != country || country == "UN" || country == "EZ" || (!region.IsCountry() && country != "ZZ") {
		return invalid("country", "invalid country")
	}
	return nil
}

func location(country, subdivision, code string, requireCurrency bool) error {
	if err := ValidateCountry(country); err != nil {
		return err
	}
	if subdivision != "" {
		if len(subdivision) < 4 || len(subdivision) > 6 || !strings.HasPrefix(subdivision, country+"-") {
			return invalid("subdivision", "invalid subdivision")
		}
		for _, c := range subdivision[3:] {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
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

var recordKinds = []string{"filing", "payment", "penalty", "notice", "other", "audit", "review"}
var recordDirections = []string{"payable", "refundable", "nil", "unknown"}
var recordStates = []string{"expected", "gathering", "ready", "filed", "assessed", "settled", "amended", "disputed", "not_required", "opened", "information_requested", "responding", "decision_issued", "appealed", "closed"}

// RecordKinds, RecordDirections and RecordStates return supported values as defensive copies.
func RecordKinds() []string      { return slices.Clone(recordKinds) }
func RecordDirections() []string { return slices.Clone(recordDirections) }
func RecordStates() []string     { return slices.Clone(recordStates) }

// InitialState returns the only state accepted on creation.
func InitialState(kind string) (string, error) {
	if !slices.Contains(recordKinds, kind) {
		return "", invalid("kind", "unknown kind")
	}
	if kind == "audit" || kind == "review" {
		return "opened", nil
	}
	return "expected", nil
}

// ValidState reports whether state is in the tax record vocabulary.
func ValidState(state string) bool { return slices.Contains(recordStates, state) }

// NormalizeTaxKind bounds and canonicalizes a tax kind.
func NormalizeTaxKind(s string) (string, error) {
	if err := text(s, "tax_kind", 256, 0, true, false); err != nil {
		return "", err
	}
	s = label(s)
	return s, ValidateTaxKind(s)
}

// ValidateTaxKind checks a canonical retained tax kind.
func ValidateTaxKind(s string) error {
	if err := text(s, "tax_kind", 256, 64, true, false); err != nil {
		return err
	}
	if label(s) != s {
		return invalid("tax_kind", "not canonical")
	}
	return nil
}

// NormalizeAccount canonicalizes and validates account input.
func NormalizeAccount(v AccountValues) (AccountValues, error) {
	if err := rawLocation(v.Country, v.Subdivision, ""); err != nil {
		return v, err
	}
	code, err := NormalizeCurrency(v.Currency)
	if err != nil {
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
	kind, err := NormalizeTaxKind(v.TaxKind)
	if err != nil {
		return v, err
	}
	v.TaxKind = kind
	v.Country = strings.ToUpper(v.Country)
	v.Subdivision = strings.ToUpper(v.Subdivision)
	v.Currency = code
	v.PeriodNote, v.DueNote, v.Note = note(v.PeriodNote), note(v.DueNote), note(v.Note)
	return v, ValidateAccount(v)
}

var accountCadences = []string{"monthly", "bimonthly", "quarterly", "semiannual", "annual", "event", "other"}

// AccountCadences returns the supported filing cadences in schema order.
func AccountCadences() []string { return slices.Clone(accountCadences) }

// ValidateAccount checks canonical retained account values.
func ValidateAccount(v AccountValues) error {
	if err := rawLocation(v.Country, v.Subdivision, v.Currency); err != nil {
		return err
	}
	if v.FilerPartyID == "" {
		return invalid("filer_party_id", "required")
	}
	if err := ValidateTaxKind(v.TaxKind); err != nil {
		return err
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
	if !slices.Contains(accountCadences, v.Cadence) {
		return invalid("cadence", "unknown cadence")
	}
	if v.LeadDays < 0 || v.LeadDays > 366 {
		return invalid("lead_days", "out of range")
	}
	return dates("effective_start", "effective_end", v.EffectiveStart, v.EffectiveEnd)
}

// NormalizeRecord canonicalizes and validates record input.
func NormalizeRecord(v RecordValues) (RecordValues, error) {
	if err := rawLocation(v.Country, v.Subdivision, ""); err != nil {
		return v, err
	}
	code, err := NormalizeCurrency(v.Currency)
	if err != nil {
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
	}{{"title", v.Title, 1200}, {"form_code", v.FormCode, 256}} {
		if err := text(item.value, item.field, item.limit, 0, false, false); err != nil {
			return v, err
		}
	}
	kind, err := NormalizeTaxKind(v.TaxKind)
	if err != nil {
		return v, err
	}
	v.Title, v.TaxKind, v.FormCode = label(v.Title), kind, label(v.FormCode)
	v.Country, v.Subdivision = strings.ToUpper(v.Country), strings.ToUpper(v.Subdivision)
	v.Currency = code
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
	}{{"title", v.Title, 1200, 300, true}, {"form_code", v.FormCode, 256, 64, false}} {
		if err := text(item.value, item.field, item.bytes, item.runes, item.required, false); err != nil {
			return err
		}
		if label(item.value) != item.value {
			return invalid(item.field, "not canonical")
		}
	}
	if err := ValidateTaxKind(v.TaxKind); err != nil {
		return err
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
	if !slices.Contains(recordDirections, v.Direction) {
		return invalid("direction", "unknown direction")
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
