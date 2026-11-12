package store

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/taxrecord"
)

func taxFixture(t *testing.T) (*Store, TaxAccount, TaxRecord) {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Example Holdings Ltd.", Own: true})
	require.NoError(t, err)
	a, err := s.CreateTaxAccount(ctx, TaxAccountInput{FilerPartyID: p.PartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"})
	require.NoError(t, err)
	r, err := s.CreateTaxRecord(ctx, TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Example", PeriodStart: new("2026-01-01"), PeriodEnd: new("2026-12-31")})
	require.NoError(t, err)
	return s, a, r
}
func TestTaxMetadataRoundTripPreservesAuthority(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	_, err := s.RetireTaxRecord(ctx, r.RecordID, r.Revision)
	require.NoError(t, err)
	_, err = s.RetireTaxAccount(ctx, a.AccountID, a.Revision)
	require.NoError(t, err)
	var original bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &original))
	require.Less(t, strings.Index(original.String(), `"type":"party"`), strings.Index(original.String(), `"type":"tax_account"`))
	require.Less(t, strings.Index(original.String(), `"type":"tax_account"`), strings.Index(original.String(), `"type":"tax_record"`))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(original.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, original.String(), restored.String())
}
func TestTaxMetadataHistoricalPartyChanges(t *testing.T) {
	t.Parallel()
	s, a, _ := taxFixture(t)
	ctx := t.Context()
	p, err := s.GetParty(ctx, a.FilerPartyID)
	require.NoError(t, err)
	p, err = s.UpdateParty(ctx, p.PartyID, p.Revision, PartyInput{Kind: "person", DisplayName: p.DisplayName})
	require.NoError(t, err)
	_, err = s.RetireParty(ctx, p.PartyID, p.Revision)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(data.Bytes())))
	_, err = target.GetTaxAccount(ctx, a.AccountID)
	require.NoError(t, err)
}
func TestTaxMetadataInvalidRowRollsBack(t *testing.T) {
	t.Parallel()
	s, a, _ := taxFixture(t)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &data))
	for _, tc := range []struct{ name, from, to, cause string }{
		{"fold", `"tax_kind_folded":"example"`, `"tax_kind_folded":"wrong"`, "invalid tax folded kind"},
		{"derived state", `"state":"expected"`, `"state":"filed"`, "state: unsupported state"},
		{"revision", `"revision":1,"created_at"`, `"revision":0,"created_at"`, "invalid tax account identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(data.String(), "\n")
			changed := false
			for i, line := range lines {
				if (strings.Contains(line, `"type":"tax_account"`) || strings.Contains(line, `"type":"tax_record"`)) && strings.Contains(line, tc.from) {
					lines[i] = strings.Replace(line, tc.from, tc.to, 1)
					changed = true
					break
				}
			}
			require.True(t, changed)
			target := newTestStore(t)
			root := target.RootID()
			vault := target.vaultID
			err := target.ImportMetadata(t.Context(), strings.NewReader(strings.Join(lines, "\n")))
			require.ErrorContains(t, err, tc.cause)
			if tc.name == "derived state" {
				require.ErrorIs(t, err, taxrecord.ErrInvalid)
			}
			assert.Equal(t, root, target.RootID())
			assert.Equal(t, vault, target.vaultID)
			_, err = target.GetParty(t.Context(), a.FilerPartyID)
			require.ErrorIs(t, err, ErrNotFound)
			rows, err := target.ListTaxAccounts(t.Context(), TaxAccountListOptions{Limit: 100})
			require.NoError(t, err)
			assert.Empty(t, rows)
		})
	}
}
func TestTaxMetadataReorderedAndTaxlessStreams(t *testing.T) {
	t.Parallel()
	s, _, _ := taxFixture(t)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &data))
	lines := strings.Split(data.String(), "\n")
	account, record := -1, -1
	for i, line := range lines {
		if strings.Contains(line, `"type":"tax_account"`) {
			account = i
		}
		if strings.Contains(line, `"type":"tax_record"`) {
			record = i
		}
	}
	require.GreaterOrEqual(t, account, 0)
	require.GreaterOrEqual(t, record, 0)
	lines[account], lines[record] = lines[record], lines[account]
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), strings.NewReader(strings.Join(lines, "\n"))))
	lines[account], lines[record] = "", ""
	// A missing tax catalog represents a snapshot predating the feature.
	old := strings.ReplaceAll(strings.Join(lines, "\n"), "\n\n", "\n")
	empty := newTestStore(t)
	require.NoError(t, empty.ImportMetadata(t.Context(), strings.NewReader(old)))
}
func TestTaxMetadataRelations(t *testing.T) {
	t.Parallel()
	s, a, _ := taxFixture(t)
	ctx := t.Context()
	other, err := s.CreateParty(ctx, PartyInput{Kind: "organization", DisplayName: "Other Example Ltd.", Own: true})
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	lines := strings.Split(data.String(), "\n")
	for i, line := range lines {
		if strings.Contains(line, `"type":"tax_record"`) {
			lines[i] = strings.Replace(line, `"filer_party_id":"`+a.FilerPartyID+`"`, `"filer_party_id":"`+other.PartyID+`"`, 1)
			break
		}
	}
	target := newTestStore(t)
	require.ErrorContains(t, target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))), "tax record account identity mismatch")
	accounts, err := target.ListTaxAccounts(ctx, TaxAccountListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, accounts)
}

func TestTaxMetadataRejectsDuplicateWhenIndexMissing(t *testing.T) {
	t.Parallel()
	s, _, _ := taxFixture(t)
	ctx := t.Context()
	_, err := s.db.ExecContext(ctx, `UPDATE tax_records SET state='filed'`)
	require.NoError(t, err)
	var invalid bytes.Buffer
	err = s.ExportMetadata(ctx, &invalid)
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	require.ErrorContains(t, err, "state: unsupported state")
	assert.Empty(t, invalid.String())
	_, err = s.db.ExecContext(ctx, `UPDATE tax_records SET state='expected'`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DROP INDEX tax_records_live_period`)
	require.NoError(t, err)
	_, columns, _ := strings.Cut(taxRecordColumns, ",")
	_, err = s.db.ExecContext(ctx, `INSERT INTO tax_records (`+taxRecordColumns+`) SELECT $1,`+columns+` FROM tax_records`, "11111111-1111-4111-8111-111111111111")
	require.NoError(t, err)
	var out bytes.Buffer
	err = s.ExportMetadata(ctx, &out)
	require.ErrorContains(t, err, "duplicate tax record period")
	assert.Empty(t, out.String())
}

func TestTaxFutureTimestampsSurviveMutationsAndRestore(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	const future = "9998-01-01T00:00:00.000000000Z"
	for _, table := range []string{"tax_accounts", "tax_records"} {
		_, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET created_at=$1, updated_at=$1", future)
		require.NoError(t, err)
	}
	ai := TaxAccountInput{FilerPartyID: a.FilerPartyID, Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	a, err := s.UpdateTaxAccount(ctx, a.AccountID, a.Revision, ai)
	require.NoError(t, err)
	require.Equal(t, future, a.UpdatedAt)
	ri := TaxRecordInput{AccountID: &a.AccountID, Kind: "filing", Title: "Edited", PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, TaxYear: r.TaxYear}
	r, err = s.UpdateTaxRecord(ctx, r.RecordID, r.Revision, ri)
	require.NoError(t, err)
	require.Equal(t, future, r.UpdatedAt)
	one, err := s.AdvanceTaxRecord(ctx, r.RecordID, r.Revision, TaxEventInput{Kind: "filed", OccurredOn: "2026-01-01"})
	require.NoError(t, err)
	two, err := s.AdvanceTaxRecord(ctx, r.RecordID, one.Record.Revision, TaxEventInput{Kind: "ready", OccurredOn: "2025-01-01"})
	require.NoError(t, err)
	assert.Equal(t, future, one.Event.RecordedAt)
	assert.Equal(t, future, two.Event.RecordedAt)
	detail, err := s.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	require.Len(t, detail.Events, 2)
	assert.Equal(t, []string{"filed", "ready"}, []string{detail.Events[0].Kind, detail.Events[1].Kind})
	assert.Equal(t, "ready", detail.Record.State)
	r = two.Record
	r, err = s.RetireTaxRecord(ctx, r.RecordID, r.Revision)
	require.NoError(t, err)
	require.Equal(t, future, *r.RetiredAt)
	a, err = s.RetireTaxAccount(ctx, a.AccountID, a.Revision)
	require.NoError(t, err)
	require.Equal(t, future, *a.RetiredAt)
	var full, backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &full))
	snap, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snap.ExportBackup(ctx, &backup))
	require.NoError(t, snap.Close())
	for _, data := range [][]byte{full.Bytes(), backup.Bytes()} {
		target := newTestStore(t)
		require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(data)))
		gotA, err := target.GetTaxAccount(ctx, a.AccountID)
		require.NoError(t, err)
		assert.Equal(t, a, gotA)
		gotR, err := target.GetTaxRecord(ctx, r.RecordID)
		require.NoError(t, err)
		assert.Equal(t, r, gotR)
	}
}

func TestTaxDuplicatePeriodImportRollsBack(t *testing.T) {
	t.Parallel()
	s, _, r := taxFixture(t)
	ctx := t.Context()
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	lines := strings.Split(data.String(), "\n")
	changed := false
	for i, line := range lines {
		if strings.Contains(line, `"type":"tax_record"`) {
			duplicate := strings.Replace(line, r.RecordID, "11111111-1111-4111-8111-111111111111", 1)
			lines[i] += "\n" + duplicate
			changed = true
			break
		}
	}
	require.True(t, changed)
	target := newTestStore(t)
	root, vault := target.RootID(), target.vaultID
	err := target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n")))
	require.Error(t, err)
	assert.True(t, target.isUniqueViolation(err), "expected a uniqueness violation")
	assert.Equal(t, root, target.RootID())
	assert.Equal(t, vault, target.vaultID)
	accounts, err := target.ListTaxAccounts(ctx, TaxAccountListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, accounts)
	records, err := target.ListTaxRecords(ctx, TaxRecordListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestTaxRestoredRevisionExhaustion(t *testing.T) {
	t.Parallel()
	s, a, r := taxFixture(t)
	ctx := t.Context()
	for _, table := range []string{"tax_accounts", "tax_records"} {
		_, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET revision=$1", int64(math.MaxInt64))
		require.NoError(t, err)
	}
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(data.Bytes())))
	beforeA, err := target.GetTaxAccount(ctx, a.AccountID)
	require.NoError(t, err)
	beforeR, err := target.GetTaxRecord(ctx, r.RecordID)
	require.NoError(t, err)
	ai := TaxAccountInput{FilerPartyID: a.FilerPartyID, Country: a.Country, TaxKind: a.TaxKind, Cadence: a.Cadence, Currency: a.Currency}
	ri := TaxRecordInput{AccountID: &a.AccountID, Kind: r.Kind, Title: r.Title, TaxYear: r.TaxYear, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd}
	_, err = target.UpdateTaxAccount(ctx, a.AccountID, math.MaxInt64, ai)
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	_, err = target.RetireTaxAccount(ctx, a.AccountID, math.MaxInt64)
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	_, err = target.UpdateTaxRecord(ctx, r.RecordID, math.MaxInt64, ri)
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	_, err = target.RetireTaxRecord(ctx, r.RecordID, math.MaxInt64)
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	_, err = target.AddTaxEvent(ctx, r.RecordID, math.MaxInt64, TaxEventInput{Kind: "note", OccurredOn: "2026-01-01"})
	require.ErrorIs(t, err, ErrTaxRevisionExhausted)
	detail, err := target.GetTaxRecordDetail(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Empty(t, detail.Events)
	afterA, err := target.GetTaxAccount(ctx, a.AccountID)
	require.NoError(t, err)
	afterR, err := target.GetTaxRecord(ctx, r.RecordID)
	require.NoError(t, err)
	assert.Equal(t, beforeA, afterA)
	assert.Equal(t, beforeR, afterR)
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, data.String(), restored.String())
}
