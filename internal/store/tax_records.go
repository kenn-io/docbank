package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/taxrecord"
)

// TaxRecord is a retained tax record and its revision.
type TaxRecord struct {
	taxrecord.RecordValues

	RecordID, TaxKindFolded, CreatedAt, UpdatedAt string
	Revision                                      int64
	RetiredAt                                     *string
}

// TaxRecordInput replaces content and nullable fields on update: nil dates,
// year, lead days and amount clear those fields. Identity and currency pointers
// retain their current values when nil. A non-nil authority is applied;
// AuthoritySet with a nil authority explicitly clears it. Kind and AccountID
// must match the existing record. Direction retains its value when empty; a
// cleared amount still has to agree with the resulting direction.
type TaxRecordInput struct {
	AccountID                                             *string
	FilerPartyID, Country, Subdivision, TaxKind, Currency *string
	AuthorityPartyID                                      *string
	AuthoritySet                                          bool
	Kind, Title, FormCode                                 string
	TaxYear                                               *int64
	PeriodStart, PeriodEnd, DueDate                       *string
	LeadDays                                              *int64
	Direction                                             string
	Amount                                                *string
	Note                                                  string
}

// TaxRecordListOptions selects a bounded page of tax records.
type TaxRecordListOptions struct {
	FilerPartyID, Country, TaxKind, State, Kind string
	TaxYear                                     *int64
	IncludeRetired                              bool
	Limit, Offset                               int
}

const taxRecordColumns = `record_id,account_id,filer_party_id,authority_party_id,kind,title,country,subdivision,tax_kind,tax_kind_folded,form_code,tax_year,period_start,period_end,due_date,lead_days,state,direction,amount_minor,currency,verification,verified_on,verified_revision,verification_note,note,revision,created_at,updated_at,retired_at`

func scanTaxRecord(row interface{ Scan(dest ...any) error }) (TaxRecord, error) {
	var r TaxRecord
	err := row.Scan(&r.RecordID, &r.AccountID, &r.FilerPartyID, &r.AuthorityPartyID, &r.Kind, &r.Title, &r.Country, &r.Subdivision, &r.TaxKind, &r.TaxKindFolded, &r.FormCode, &r.TaxYear, &r.PeriodStart, &r.PeriodEnd, &r.DueDate, &r.LeadDays, &r.State, &r.Direction, &r.AmountMinor, &r.Currency, &r.Verification, &r.VerifiedOn, &r.VerifiedRevision, &r.VerificationNote, &r.Note, &r.Revision, &r.CreatedAt, &r.UpdatedAt, &r.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaxRecord{}, ErrNotFound
	}
	return r, err
}

const taxRecordUpdateColumns = `record_id,filer_party_id,authority_party_id,title,country,subdivision,tax_kind,tax_kind_folded,form_code,tax_year,period_start,period_end,due_date,lead_days,direction,amount_minor,currency,note,revision,updated_at,retired_at`

func taxRecordUpdateArgs(r TaxRecord) []any {
	return []any{r.RecordID, r.FilerPartyID, r.AuthorityPartyID, r.Title, r.Country, r.Subdivision, r.TaxKind, r.TaxKindFolded, r.FormCode, r.TaxYear, r.PeriodStart, r.PeriodEnd, r.DueDate, r.LeadDays, r.Direction, r.AmountMinor, r.Currency, r.Note, r.Revision, r.UpdatedAt, r.RetiredAt}
}
func taxRecordArgs(r TaxRecord) []any {
	return []any{r.RecordID, r.AccountID, r.FilerPartyID, r.AuthorityPartyID, r.Kind, r.Title, r.Country, r.Subdivision, r.TaxKind, r.TaxKindFolded, r.FormCode, r.TaxYear, r.PeriodStart, r.PeriodEnd, r.DueDate, r.LeadDays, r.State, r.Direction, r.AmountMinor, r.Currency, r.Verification, r.VerifiedOn, r.VerifiedRevision, r.VerificationNote, r.Note, r.Revision, r.CreatedAt, r.UpdatedAt, r.RetiredAt}
}
func getTaxRecord(ctx context.Context, q rowQuerier, id string) (TaxRecord, error) {
	if validateUUIDv4(id) != nil {
		return TaxRecord{}, ErrNotFound
	}
	return scanTaxRecord(q.QueryRowContext(ctx, `SELECT `+taxRecordColumns+` FROM tax_records WHERE record_id=$1`, id))
}

// GetTaxRecord returns a tax record, including retired rows.
func (s *Store) GetTaxRecord(ctx context.Context, id string) (TaxRecord, error) {
	return getTaxRecord(ctx, s.db, id)
}
func chooseTaxString(in *string, fallback string) string {
	if in == nil {
		return fallback
	}
	return *in
}
func taxRecordValues(in TaxRecordInput, old *TaxRecord, account *TaxAccount) (taxrecord.RecordValues, error) {
	var v taxrecord.RecordValues
	if old != nil {
		v = old.RecordValues
	} else {
		v.AccountID = in.AccountID
		v.Kind = in.Kind
		state, err := taxrecord.InitialState(in.Kind)
		if err != nil {
			return v, err
		}
		v.State = state
		v.Verification = "unverified"
		v.Direction = "unknown"
	}
	if account != nil && old == nil {
		v.FilerPartyID = account.FilerPartyID
		v.AuthorityPartyID = account.AuthorityPartyID
		v.Country = account.Country
		v.Subdivision = account.Subdivision
		v.TaxKind = account.TaxKind
		v.Currency = account.Currency
	}
	v.FilerPartyID = chooseTaxString(in.FilerPartyID, v.FilerPartyID)
	v.Country = chooseTaxString(in.Country, v.Country)
	v.Subdivision = chooseTaxString(in.Subdivision, v.Subdivision)
	v.TaxKind = chooseTaxString(in.TaxKind, v.TaxKind)
	v.Currency = chooseTaxString(in.Currency, v.Currency)
	if in.AuthoritySet || in.AuthorityPartyID != nil {
		v.AuthorityPartyID = in.AuthorityPartyID
	}
	v.Title = in.Title
	v.FormCode = in.FormCode
	v.TaxYear = in.TaxYear
	v.PeriodStart = in.PeriodStart
	v.PeriodEnd = in.PeriodEnd
	v.DueDate = in.DueDate
	v.LeadDays = in.LeadDays
	v.Note = in.Note
	if in.Direction != "" {
		v.Direction = in.Direction
	} else if old == nil {
		v.Direction = "unknown"
	}
	v.AmountMinor = nil
	if in.Amount != nil {
		code, err := taxrecord.NormalizeCurrency(v.Currency)
		if err != nil {
			return v, err
		}
		amount, err := taxrecord.ParseMoney(*in.Amount, code)
		if err != nil {
			return v, err
		}
		v.AmountMinor = &amount
	}
	if old == nil && v.TaxYear == nil && v.PeriodEnd != nil && len(*v.PeriodEnd) >= 4 {
		year, err := strconv.ParseInt((*v.PeriodEnd)[:4], 10, 64)
		if err == nil {
			v.TaxYear = &year
		}
	}
	return taxrecord.NormalizeRecord(v)
}
func taxRecordIdentityError(v, expected taxrecord.RecordValues) error {
	for _, item := range []struct {
		field string
		equal bool
	}{{"filer_party_id", v.FilerPartyID == expected.FilerPartyID}, {"authority_party_id", equalOptionalStrings(v.AuthorityPartyID, expected.AuthorityPartyID)}, {"country", v.Country == expected.Country}, {"subdivision", v.Subdivision == expected.Subdivision}, {"tax_kind", v.TaxKind == expected.TaxKind}} {
		if !item.equal {
			return &taxrecord.FieldError{Field: item.field, Reason: "account identity mismatch"}
		}
	}
	return nil
}
func (s *Store) taxRecordWriteError(err error) error {
	if err != nil && s.isUniqueViolation(err) {
		return fmt.Errorf("tax record period already exists: %w", ErrExists)
	}
	return err
}

// CreateTaxRecord creates a tax record in a logical transaction.
func (s *Store) CreateTaxRecord(ctx context.Context, in TaxRecordInput) (TaxRecord, error) {
	id, err := newUUIDv4()
	if err != nil {
		return TaxRecord{}, err
	}
	var result TaxRecord
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var account *TaxAccount
		if in.AccountID != nil {
			a, err := getTaxAccount(ctx, tx, *in.AccountID)
			if err != nil {
				return err
			}
			if a.RetiredAt != nil {
				return ErrTaxRetired
			}
			account = &a
		}
		v, err := taxRecordValues(in, nil, account)
		if err != nil {
			return err
		}
		if account != nil {
			expected := taxrecord.RecordValues{FilerPartyID: account.FilerPartyID, AuthorityPartyID: account.AuthorityPartyID, Country: account.Country, Subdivision: account.Subdivision, TaxKind: account.TaxKind}
			if err := taxRecordIdentityError(v, expected); err != nil {
				return err
			}
		}
		if err := checkTaxParties(ctx, tx, v.FilerPartyID, v.AuthorityPartyID, "", nil); err != nil {
			return err
		}
		now := nowRFC3339()
		r := TaxRecord{RecordValues: v, RecordID: id, TaxKindFolded: document.FoldPersonName(v.TaxKind), Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := taxInsert(ctx, tx, "tax_records", taxRecordColumns, taxRecordArgs(r)); err != nil {
			return s.taxRecordWriteError(err)
		}
		result = r
		return nil
	})
	return result, err
}

// UpdateTaxRecord replaces editable fields at the expected revision.
func (s *Store) UpdateTaxRecord(ctx context.Context, id string, revision int64, in TaxRecordInput) (TaxRecord, error) {
	var result TaxRecord
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		r, err := getTaxRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Revision != revision {
			return ErrStaleRevision
		}
		if r.RetiredAt != nil {
			return ErrTaxRetired
		}
		if !equalOptionalStrings(in.AccountID, r.AccountID) || in.Kind != r.Kind {
			return fmt.Errorf("%w: record binding and kind are immutable", taxrecord.ErrInvalid)
		}
		v, err := taxRecordValues(in, &r, nil)
		if err != nil {
			return err
		}
		if r.AccountID != nil {
			if err := taxRecordIdentityError(v, r.RecordValues); err != nil {
				return err
			}
		}
		if err := checkTaxParties(ctx, tx, v.FilerPartyID, v.AuthorityPartyID, r.FilerPartyID, r.AuthorityPartyID); err != nil {
			return err
		}
		events, err := getTaxRecordEvents(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err = taxrecord.CalculateBalance(v, taxEventFacts(events)); err != nil {
			return err
		}
		r.RecordValues = v
		r.TaxKindFolded = document.FoldPersonName(v.TaxKind)
		if err := advanceTaxRevision(&r.Revision); err != nil {
			return err
		}
		r.UpdatedAt = max(nowRFC3339(), r.UpdatedAt)
		if err := taxUpdate(ctx, tx, "tax_records", taxRecordUpdateColumns, taxRecordUpdateArgs(r), revision); err != nil {
			return s.taxRecordWriteError(err)
		}
		result = r
		return nil
	})
	return result, err
}

// RetireTaxRecord retires the row at the expected revision.
func (s *Store) RetireTaxRecord(ctx context.Context, id string, revision int64) (TaxRecord, error) {
	var result TaxRecord
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		r, err := getTaxRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Revision != revision {
			return ErrStaleRevision
		}
		if r.RetiredAt != nil {
			return ErrTaxRetired
		}
		now := max(nowRFC3339(), r.UpdatedAt)
		r.RetiredAt = &now
		r.UpdatedAt = now
		if err := advanceTaxRevision(&r.Revision); err != nil {
			return err
		}
		if err := taxUpdate(ctx, tx, "tax_records", taxRecordUpdateColumns, taxRecordUpdateArgs(r), revision); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

// ListTaxRecords returns the requested page in stable ID order.
func (s *Store) ListTaxRecords(ctx context.Context, o TaxRecordListOptions) ([]TaxRecord, error) {
	if err := validatePage(o.Limit, o.Offset); err != nil {
		return nil, err
	}
	if err := validateTaxSelectors(o.FilerPartyID, o.Country, o.TaxKind); err != nil {
		return nil, err
	}
	if o.TaxYear != nil && (*o.TaxYear < 1 || *o.TaxYear > 9999) {
		return nil, fmt.Errorf("%w: tax year selector", taxrecord.ErrInvalid)
	}
	if o.Kind != "" {
		if _, err := taxrecord.InitialState(o.Kind); err != nil {
			return nil, err
		}
	}
	if o.State != "" && !taxrecord.ValidState(o.State) {
		return nil, &taxrecord.FieldError{Field: "state", Reason: "unknown state"}
	}
	year := int64(0)
	if o.TaxYear != nil {
		year = *o.TaxYear
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taxRecordColumns+` FROM tax_records WHERE ($1='' OR filer_party_id=$1) AND ($2='' OR country=$2) AND ($3='' OR tax_kind_folded=$4) AND ($5 OR tax_year=$6) AND ($7='' OR state=$7) AND ($8='' OR kind=$8) AND ($9 OR retired_at IS NULL) ORDER BY record_id LIMIT $10 OFFSET $11`, o.FilerPartyID, o.Country, o.TaxKind, document.FoldPersonName(o.TaxKind), o.TaxYear == nil, year, o.State, o.Kind, o.IncludeRetired, o.Limit, o.Offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []TaxRecord{}
	for rows.Next() {
		r, err := scanTaxRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
