package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/taxrecord"
)

// ErrTaxRetired indicates that a retired item cannot accept a mutation.
var ErrTaxRetired = errors.New("tax item is retired")

// TaxAccount is a retained tax account and its revision.
type TaxAccount struct {
	taxrecord.AccountValues
	AccountID, TaxKindFolded, CreatedAt, UpdatedAt string
	Revision                                       int64
	RetiredAt                                      *string
}

// TaxAccountInput replaces account fields on update; identity must match and
// a nil LeadDays retains the current value.
type TaxAccountInput struct {
	FilerPartyID                                                            string
	AuthorityPartyID                                                        *string
	Country, Subdivision, TaxKind, Identifier, Cadence, PeriodNote, DueNote string
	LeadDays                                                                *int64
	Currency                                                                string
	EffectiveStart, EffectiveEnd                                            *string
	Note                                                                    string
}

// TaxAccountListOptions selects a bounded page of tax accounts.
type TaxAccountListOptions struct {
	FilerPartyID, Country, TaxKind string
	IncludeRetired                 bool
	Limit, Offset                  int
}

const taxAccountColumns = `account_id,filer_party_id,authority_party_id,country,subdivision,tax_kind,tax_kind_folded,identifier,cadence,period_note,due_note,lead_days,currency,effective_start,effective_end,note,revision,created_at,updated_at,retired_at`

func scanTaxAccount(row interface{ Scan(...any) error }) (TaxAccount, error) {
	var a TaxAccount
	err := row.Scan(&a.AccountID, &a.FilerPartyID, &a.AuthorityPartyID, &a.Country, &a.Subdivision, &a.TaxKind, &a.TaxKindFolded, &a.Identifier, &a.Cadence, &a.PeriodNote, &a.DueNote, &a.LeadDays, &a.Currency, &a.EffectiveStart, &a.EffectiveEnd, &a.Note, &a.Revision, &a.CreatedAt, &a.UpdatedAt, &a.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaxAccount{}, ErrNotFound
	}
	return a, err
}

const taxAccountUpdateColumns = `account_id,identifier,cadence,period_note,due_note,lead_days,currency,effective_start,effective_end,note,revision,updated_at,retired_at`

func taxAccountUpdateArgs(a TaxAccount) []any {
	return []any{a.AccountID, a.Identifier, a.Cadence, a.PeriodNote, a.DueNote, a.LeadDays, a.Currency, a.EffectiveStart, a.EffectiveEnd, a.Note, a.Revision, a.UpdatedAt, a.RetiredAt}
}
func taxAccountArgs(a TaxAccount) []any {
	return []any{a.AccountID, a.FilerPartyID, a.AuthorityPartyID, a.Country, a.Subdivision, a.TaxKind, a.TaxKindFolded, a.Identifier, a.Cadence, a.PeriodNote, a.DueNote, a.LeadDays, a.Currency, a.EffectiveStart, a.EffectiveEnd, a.Note, a.Revision, a.CreatedAt, a.UpdatedAt, a.RetiredAt}
}
func getTaxAccount(ctx context.Context, q rowQuerier, id string) (TaxAccount, error) {
	if validateUUIDv4(id) != nil {
		return TaxAccount{}, ErrNotFound
	}
	return scanTaxAccount(q.QueryRowContext(ctx, `SELECT `+taxAccountColumns+` FROM tax_accounts WHERE account_id=$1`, id))
}

// GetTaxAccount returns a tax account, including retired rows.
func (s *Store) GetTaxAccount(ctx context.Context, id string) (TaxAccount, error) {
	return getTaxAccount(ctx, s.db, id)
}

// taxInsert and taxUpdate use only compile-time table and column names supplied by their callers.
func taxInsert(ctx context.Context, tx *sql.Tx, table, columns string, args []any) error {
	markers := make([]string, len(args))
	for i := range markers {
		markers[i] = fmt.Sprintf("$%d", i+1)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (`+columns+`) VALUES (`+strings.Join(markers, ",")+`)`, args...)
	return err
}
func taxUpdate(ctx context.Context, tx *sql.Tx, table, columns string, args []any, revision int64) error {
	cols := strings.Split(columns, ",")
	sets := make([]string, 0, len(cols)-1)
	for i := 1; i < len(cols); i++ {
		sets = append(sets, fmt.Sprintf("%s=$%d", cols[i], i))
	}
	values := append(append([]any{}, args[1:]...), args[0], revision)
	result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET `+strings.Join(sets, ",")+fmt.Sprintf(` WHERE %s=$%d AND revision=$%d`, cols[0], len(args), len(args)+1), values...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStaleRevision
	}
	return nil
}
func sameString(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func checkTaxParty(ctx context.Context, tx *sql.Tx, id string, filer bool) error {
	field := "authority_party_id"
	if filer {
		field = "filer_party_id"
	}
	p, err := getPartyQuery(ctx, tx, id)
	if errors.Is(err, ErrNotFound) {
		return &taxrecord.FieldError{Field: field, Reason: "party not found"}
	}
	if err != nil {
		return err
	}
	if p.RetiredAt != nil {
		return &taxrecord.FieldError{Field: field, Reason: "party is retired"}
	}
	if filer && !p.Own {
		return &taxrecord.FieldError{Field: field, Reason: "party is not own"}
	}
	if !filer && p.Kind != "organization" {
		return &taxrecord.FieldError{Field: field, Reason: "party is not an organization"}
	}
	return nil
}
func checkTaxParties(ctx context.Context, tx *sql.Tx, filer string, authority *string, oldFiler string, oldAuthority *string) error {
	if filer != oldFiler {
		if err := checkTaxParty(ctx, tx, filer, true); err != nil {
			return err
		}
	}
	if authority != nil && !sameString(authority, oldAuthority) {
		if err := checkTaxParty(ctx, tx, *authority, false); err != nil {
			return err
		}
	}
	return nil
}
func accountValues(in TaxAccountInput, lead int64) taxrecord.AccountValues {
	return taxrecord.AccountValues{FilerPartyID: in.FilerPartyID, AuthorityPartyID: in.AuthorityPartyID, Country: in.Country, Subdivision: in.Subdivision, TaxKind: in.TaxKind, Identifier: in.Identifier, Cadence: in.Cadence, PeriodNote: in.PeriodNote, DueNote: in.DueNote, LeadDays: lead, Currency: in.Currency, EffectiveStart: in.EffectiveStart, EffectiveEnd: in.EffectiveEnd, Note: in.Note}
}

// CreateTaxAccount creates a tax account in a logical transaction.
func (s *Store) CreateTaxAccount(ctx context.Context, in TaxAccountInput) (TaxAccount, error) {
	lead := int64(14)
	if in.LeadDays != nil {
		lead = *in.LeadDays
	}
	v, err := taxrecord.NormalizeAccount(accountValues(in, lead))
	if err != nil {
		return TaxAccount{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return TaxAccount{}, err
	}
	now := nowRFC3339()
	a := TaxAccount{AccountValues: v, AccountID: id, TaxKindFolded: document.FoldPersonName(v.TaxKind), Revision: 1, CreatedAt: now, UpdatedAt: now}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := checkTaxParties(ctx, tx, a.FilerPartyID, a.AuthorityPartyID, "", nil); err != nil {
			return err
		}
		return taxInsert(ctx, tx, "tax_accounts", taxAccountColumns, taxAccountArgs(a))
	})
	if err != nil {
		return TaxAccount{}, err
	}
	return a, nil
}

// UpdateTaxAccount replaces editable fields at the expected revision.
func (s *Store) UpdateTaxAccount(ctx context.Context, id string, revision int64, in TaxAccountInput) (TaxAccount, error) {
	var result TaxAccount
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		a, err := getTaxAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.Revision != revision {
			return ErrStaleRevision
		}
		if a.RetiredAt != nil {
			return ErrTaxRetired
		}
		lead := a.LeadDays
		if in.LeadDays != nil {
			lead = *in.LeadDays
		}
		v, err := taxrecord.NormalizeAccount(accountValues(in, lead))
		if err != nil {
			return err
		}
		if v.FilerPartyID != a.FilerPartyID || !sameString(v.AuthorityPartyID, a.AuthorityPartyID) || v.Country != a.Country || v.Subdivision != a.Subdivision || v.TaxKind != a.TaxKind {
			return fmt.Errorf("%w: account identity is immutable", taxrecord.ErrInvalid)
		}
		a.AccountValues = v
		a.Revision++
		a.UpdatedAt = max(nowRFC3339(), a.UpdatedAt)
		if err := taxUpdate(ctx, tx, "tax_accounts", taxAccountUpdateColumns, taxAccountUpdateArgs(a), revision); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}

// RetireTaxAccount retires the row at the expected revision.
func (s *Store) RetireTaxAccount(ctx context.Context, id string, revision int64) (TaxAccount, error) {
	var result TaxAccount
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		a, err := getTaxAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.Revision != revision {
			return ErrStaleRevision
		}
		if a.RetiredAt != nil {
			return ErrTaxRetired
		}
		now := max(nowRFC3339(), a.UpdatedAt)
		a.RetiredAt = &now
		a.UpdatedAt = now
		a.Revision++
		if err := taxUpdate(ctx, tx, "tax_accounts", taxAccountUpdateColumns, taxAccountUpdateArgs(a), revision); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}
func validateTaxSelectors(filer, country, kind string) error {
	if filer != "" && validateUUIDv4(filer) != nil {
		return fmt.Errorf("%w: filer selector", taxrecord.ErrInvalid)
	}
	if country != "" {
		if err := taxrecord.ValidateCountry(country); err != nil {
			return err
		}
	}
	if kind != "" {
		if _, err := taxrecord.NormalizeTaxKind(kind); err != nil {
			return err
		}
	}
	return nil
}

// ListTaxAccounts returns the requested page in stable ID order.
func (s *Store) ListTaxAccounts(ctx context.Context, o TaxAccountListOptions) ([]TaxAccount, error) {
	if err := validatePage(o.Limit, o.Offset); err != nil {
		return nil, err
	}
	if err := validateTaxSelectors(o.FilerPartyID, o.Country, o.TaxKind); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taxAccountColumns+` FROM tax_accounts WHERE ($1='' OR filer_party_id=$1) AND ($2='' OR country=$2) AND ($3='' OR tax_kind_folded=$4) AND ($5 OR retired_at IS NULL) ORDER BY account_id LIMIT $6 OFFSET $7`, o.FilerPartyID, o.Country, o.TaxKind, document.FoldPersonName(o.TaxKind), o.IncludeRetired, o.Limit, o.Offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []TaxAccount{}
	for rows.Next() {
		a, err := scanTaxAccount(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
