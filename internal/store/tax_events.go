package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/docbank/internal/taxrecord"
)

// ErrTaxEventVoided indicates an event has already been voided.
var ErrTaxEventVoided = errors.New("tax event already voided")

// TaxEvent contains exact retained event authority, including its reference.
type TaxEvent struct {
	taxrecord.EventValues
	EventID, RecordID, RecordedAt, VoidReason string
	RecordRevision                            int64
	VoidedAt                                  *string
}

// TaxEventInput is the event content supplied by a caller.
type TaxEventInput struct {
	Kind, OccurredOn, Currency, Reference, Note string
	Amount                                      *string
}

// TaxEventMutation returns the updated parent and affected event.
type TaxEventMutation struct {
	Record TaxRecord
	Event  TaxEvent
}

// TaxRecordDetail contains a parent and its events in append order.
type TaxRecordDetail struct {
	Record TaxRecord
	Events []TaxEvent
}

const taxEventColumns = `event_id,record_id,kind,occurred_on,sets_state,amount_minor,currency,reference,note,recorded_at,voided_at,void_reason,record_revision`

func scanTaxEvent(row interface{ Scan(...any) error }) (TaxEvent, error) {
	var e TaxEvent
	err := row.Scan(&e.EventID, &e.RecordID, &e.Kind, &e.OccurredOn, &e.SetsState, &e.AmountMinor, &e.Currency, &e.Reference, &e.Note, &e.RecordedAt, &e.VoidedAt, &e.VoidReason, &e.RecordRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}
func taxEventFacts(events []TaxEvent) []taxrecord.EventFact {
	facts := make([]taxrecord.EventFact, 0, len(events))
	for _, e := range events {
		facts = append(facts, taxrecord.EventFact{EventValues: e.EventValues, EventID: e.EventID, RecordRevision: e.RecordRevision, Voided: e.VoidedAt != nil})
	}
	return facts
}
func getTaxRecordDetail(ctx context.Context, q metadataQuerier, id string) (TaxRecordDetail, error) {
	r, err := getTaxRecord(ctx, q, id)
	if err != nil {
		return TaxRecordDetail{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+taxEventColumns+` FROM tax_events WHERE record_id=$1 ORDER BY record_revision`, id)
	if err != nil {
		return TaxRecordDetail{}, err
	}
	defer rows.Close()
	d := TaxRecordDetail{Record: r, Events: []TaxEvent{}}
	for rows.Next() {
		e, err := scanTaxEvent(rows)
		if err != nil {
			return TaxRecordDetail{}, err
		}
		d.Events = append(d.Events, e)
	}
	return d, rows.Err()
}

// GetTaxRecordDetail reads the parent and children from one snapshot.
func (s *Store) GetTaxRecordDetail(ctx context.Context, id string) (TaxRecordDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return TaxRecordDetail{}, err
	}
	defer func() { _ = tx.Rollback() }()
	d, err := getTaxRecordDetail(ctx, tx, id)
	if err != nil {
		return TaxRecordDetail{}, err
	}
	if err = tx.Commit(); err != nil {
		return TaxRecordDetail{}, err
	}
	return d, nil
}

// AdvanceTaxRecord appends a state event and advances the parent atomically.
func (s *Store) AdvanceTaxRecord(ctx context.Context, id string, revision int64, in TaxEventInput) (TaxEventMutation, error) {
	return s.appendTaxEvent(ctx, id, revision, in, true)
}

// AddTaxEvent appends a note without changing state.
func (s *Store) AddTaxEvent(ctx context.Context, id string, revision int64, in TaxEventInput) (TaxEventMutation, error) {
	return s.appendTaxEvent(ctx, id, revision, in, false)
}
func (s *Store) appendTaxEvent(ctx context.Context, id string, revision int64, in TaxEventInput, advance bool) (TaxEventMutation, error) {
	var result TaxEventMutation
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
		v := taxrecord.EventValues{Kind: in.Kind, OccurredOn: in.OccurredOn, Currency: in.Currency, Reference: in.Reference, Note: in.Note}
		if advance {
			if in.Kind == "note" {
				return taxrecord.ErrInvalid
			}
			v.SetsState = in.Kind
		} else if in.Kind != "note" {
			return taxrecord.ErrInvalid
		}
		if in.Amount != nil {
			code, err := taxrecord.NormalizeCurrency(in.Currency)
			if err != nil {
				return err
			}
			amount, err := taxrecord.ParseMoney(*in.Amount, code)
			if err != nil {
				return err
			}
			v.AmountMinor = &amount
		}
		v, err = taxrecord.NormalizeEvent(r.Kind, v)
		if err != nil {
			return err
		}
		if err = advanceTaxRevision(&r.Revision); err != nil {
			return err
		}
		r.UpdatedAt = max(nowRFC3339(), r.UpdatedAt)
		if advance {
			r.State = v.SetsState
		}
		eventID, err := newUUIDv4()
		if err != nil {
			return err
		}
		e := TaxEvent{EventValues: v, EventID: eventID, RecordID: id, RecordedAt: r.UpdatedAt, RecordRevision: r.Revision}
		if err = taxInsert(ctx, tx, "tax_events", taxEventColumns, []any{e.EventID, e.RecordID, e.Kind, e.OccurredOn, e.SetsState, e.AmountMinor, e.Currency, e.Reference, e.Note, e.RecordedAt, e.VoidedAt, e.VoidReason, e.RecordRevision}); err != nil {
			return err
		}
		if err = updateTaxEventParent(ctx, tx, r, revision); err != nil {
			return err
		}
		result = TaxEventMutation{Record: r, Event: e}
		return nil
	})
	if err != nil {
		return TaxEventMutation{}, err
	}
	return result, nil
}
func updateTaxEventParent(ctx context.Context, tx *sql.Tx, r TaxRecord, revision int64) error {
	return taxUpdate(ctx, tx, "tax_records", "record_id,state,revision,updated_at", []any{r.RecordID, r.State, r.Revision, r.UpdatedAt}, revision)
}

// VoidTaxEvent voids one event and recalculates state in the same transaction.
func (s *Store) VoidTaxEvent(ctx context.Context, recordID, eventID string, revision int64, reason string) (TaxEventMutation, error) {
	var result TaxEventMutation
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		d, err := getTaxRecordDetail(ctx, tx, recordID)
		if err != nil {
			return err
		}
		r := d.Record
		if r.Revision != revision {
			return ErrStaleRevision
		}
		if r.RetiredAt != nil {
			return ErrTaxRetired
		}
		if validateUUIDv4(eventID) != nil {
			return ErrNotFound
		}
		var target *TaxEvent
		for i := range d.Events {
			if d.Events[i].EventID == eventID {
				target = &d.Events[i]
				break
			}
		}
		if target == nil {
			return ErrNotFound
		}
		if target.VoidedAt != nil {
			return ErrTaxEventVoided
		}
		reason, err = taxrecord.NormalizeVoidReason(reason)
		if err != nil {
			return err
		}
		if err = advanceTaxRevision(&r.Revision); err != nil {
			return err
		}
		r.UpdatedAt = max(nowRFC3339(), r.UpdatedAt)
		target.VoidedAt = &r.UpdatedAt
		target.VoidReason = reason
		r.State, err = taxrecord.StateFromEvents(r.Kind, taxEventFacts(d.Events))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tax_events SET voided_at=$1,void_reason=$2 WHERE event_id=$3`, target.VoidedAt, reason, eventID); err != nil {
			return err
		}
		if err = updateTaxEventParent(ctx, tx, r, revision); err != nil {
			return err
		}
		result = TaxEventMutation{Record: r, Event: *target}
		return nil
	})
	if err != nil {
		return TaxEventMutation{}, err
	}
	return result, nil
}
