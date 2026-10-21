package store

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"go.kenn.io/docbank/internal/audit"
)

const (
	auditPhotoRepresentativeKind = "photo_representative"
	auditPhotoBurstSettingsKind  = "photo_burst_settings"
)

type photoBurstSettings struct{ gap, sequence uint64 }

func photoBurstSettingsTx(ctx context.Context, tx metadataQuerier) (photoBurstSettings, error) {
	state := photoBurstSettings{gap: 2}
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(burst_gap_seconds,2),COALESCE(representative_sequence,0) FROM photo_library_settings WHERE singleton=1`).Scan(&state.gap, &state.sequence)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return state, err
}

func photoBurstSettingsRecord(state photoBurstSettings) audit.Record {
	if state == (photoBurstSettings{gap: 2}) {
		return audit.Record{}
	}
	return audit.Record{Kind: auditPhotoBurstSettingsKind, Fields: []audit.Field{
		{Name: "singleton", Value: audit.Unsigned(1)},
		{Name: "gap", Value: audit.Unsigned(state.gap)},
		{Name: "sequence", Value: audit.Unsigned(state.sequence)},
	}}
}

func photoRepresentativeRecord(id string, rank *int64) (audit.Record, error) {
	if rank == nil {
		return audit.Record{}, nil
	}
	if *rank <= 0 {
		return audit.Record{}, errors.New("invalid photo representative rank")
	}
	value, err := audit.UUID(id)
	return audit.Record{Kind: auditPhotoRepresentativeKind, Fields: []audit.Field{
		{Name: "asset_id", Value: value}, {Name: "rank", Value: audit.Unsigned(uint64(*rank))},
	}}, err
}

func appendAuditPhotoBurst(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	settings, err := photoBurstSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	if record := photoBurstSettingsRecord(settings); record.Kind != "" {
		*records = append(*records, record)
	}
	rows, err := tx.QueryContext(ctx, `SELECT asset_id,representative_rank FROM photo_assets WHERE representative_rank IS NOT NULL ORDER BY asset_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var rank int64
		if err := rows.Scan(&id, &rank); err != nil {
			return err
		}
		record, err := photoRepresentativeRecord(id, &rank)
		if err != nil {
			return err
		}
		*records = append(*records, record)
	}
	return rows.Err()
}

func (s *Store) auditPhotoBurstTx(ctx context.Context, tx *sql.Tx, prior photoBurstSettings, representatives []PhotoRepresentativeChange) error {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil || !active {
		return err
	}
	current, err := photoBurstSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	var changes []audit.Record
	appendChange := func(pre, post audit.Record) error {
		if pre.Kind == "" && post.Kind == "" || auditRecordEqual(pre, post) {
			return nil
		}
		change, err := makeAttachedMetadataChange(pre, post)
		if err == nil {
			changes = append(changes, change)
		}
		return err
	}
	if err := appendChange(photoBurstSettingsRecord(prior), photoBurstSettingsRecord(current)); err != nil {
		return err
	}
	for _, representative := range representatives {
		pre, err := photoRepresentativeRecord(representative.AssetID, representative.Before)
		if err != nil {
			return err
		}
		post, err := photoRepresentativeRecord(representative.AssetID, representative.After)
		if err != nil {
			return err
		}
		if err := appendChange(pre, post); err != nil {
			return err
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return s.persistAuditedGlobalMetadataChanges(ctx, tx, changes)
}

func validatePhotoBurstRecord(record audit.Record) error {
	switch record.Kind {
	case auditPhotoRepresentativeKind:
		if _, err := auditUUIDField(record, "asset_id"); err != nil {
			return err
		}
		rank, err := auditUnsignedField(record, "rank")
		if err != nil || rank == 0 || rank > math.MaxInt64 {
			return errors.New("invalid photo representative rank")
		}
	case auditPhotoBurstSettingsKind:
		if err := requireAuditUnsigned(record, "singleton", 1); err != nil {
			return err
		}
		gap, err := auditUnsignedField(record, "gap")
		if err != nil || gap < 1 || gap > 30 {
			return errors.New("invalid audited burst gap")
		}
		sequence, err := auditUnsignedField(record, "sequence")
		if err != nil || sequence > math.MaxInt64 || gap == 2 && sequence == 0 {
			return errors.New("invalid audited representative sequence or explicit default")
		}
	default:
		return errors.New("invalid photo burst record kind")
	}
	return nil
}

func (replay *auditedHistoryReplay) applyUnscopedPhotoBurstChanges(
	operationID, digest string, allocation storedAuditRecord, nextCount int64,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (bool, error) {
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return false, nil
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) == 0 {
		return false, err
	}
	kind, err := auditTextField(changes[0], "record_kind")
	if err != nil || kind != auditPhotoRepresentativeKind && kind != auditPhotoBurstSettingsKind {
		return false, err
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return true, err
	}
	if err := requireAuditUnsigned(allocation.record, auditAttachedMetadataChangeCountField, uint64(len(changes))); err != nil {
		return true, err
	}
	priorSettings := photoBurstSettings{gap: 2}
	settingsKey, err := attachedAuditKey(photoBurstSettingsRecord(photoBurstSettings{gap: 2, sequence: 1}))
	if err != nil {
		return true, err
	}
	if record, exists := replay.attachments[settingsKey]; exists {
		gap, _ := auditUnsignedField(record, "gap")
		sequence, _ := auditUnsignedField(record, "sequence")
		priorSettings = photoBurstSettings{gap, sequence}
	}
	nextSettings := priorSettings
	pending := map[string]audit.Record{}
	var representativeCount, settingsCount int
	var rank uint64
	for _, change := range changes {
		kind, err := auditTextField(change, "record_kind")
		if err != nil || kind != auditPhotoRepresentativeKind && kind != auditPhotoBurstSettingsKind {
			return true, errors.New("mixed photo burst metadata changes")
		}
		pre, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
		if err != nil {
			return true, err
		}
		post, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
		if err != nil || !hasPre && !hasPost {
			return true, errors.New("photo burst change lacks state")
		}
		record := post
		if !hasPost {
			record = pre
		}
		identity, err := attachedAuditIdentity(record)
		if err != nil {
			return true, err
		}
		storedIdentity, err := auditNestedField(change, "stable_identity")
		if err != nil || !auditRecordEqual(storedIdentity, identity) {
			return true, errors.New("photo burst identity differs from record")
		}
		for _, state := range []audit.Record{pre, post} {
			if state.Kind == "" {
				continue
			}
			if state.Kind != kind {
				return true, errors.New("photo burst record kind differs from change")
			}
			if err := validatePhotoBurstRecord(state); err != nil {
				return true, err
			}
			stateIdentity, err := attachedAuditIdentity(state)
			if err != nil || !auditRecordEqual(stateIdentity, identity) {
				return true, errors.New("photo burst change alters identity")
			}
		}
		key, err := attachedAuditKey(record)
		if err != nil {
			return true, err
		}
		if _, exists := pending[key]; exists {
			return true, errors.New("duplicate photo burst transition")
		}
		current, exists := replay.attachments[key]
		if hasPre != exists || hasPre && !auditRecordEqual(current, pre) {
			return true, errors.New("photo burst pre-state differs from replay")
		}
		if hasPre && hasPost && auditRecordEqual(pre, post) {
			return true, errors.New("photo burst transition changes no state")
		}
		pending[key] = post
		if kind == auditPhotoBurstSettingsKind {
			settingsCount++
			nextSettings = photoBurstSettings{gap: 2}
			if hasPost {
				gap, _ := auditUnsignedField(post, "gap")
				sequence, _ := auditUnsignedField(post, "sequence")
				nextSettings = photoBurstSettings{gap, sequence}
			}
		} else {
			representativeCount++
			if hasPost {
				rank, _ = auditUnsignedField(post, "rank")
			}
		}
	}
	valid := false
	switch {
	case settingsCount == 1 && representativeCount == 1:
		valid = nextSettings.gap == priorSettings.gap && priorSettings.sequence < math.MaxInt64 && nextSettings.sequence == priorSettings.sequence+1 && rank == nextSettings.sequence
	case settingsCount == 1 && representativeCount == 0:
		valid = nextSettings.sequence == priorSettings.sequence && nextSettings.gap != priorSettings.gap
	case settingsCount == 0 && representativeCount == 1:
		valid = rank <= priorSettings.sequence
	}
	if !valid {
		return true, errors.New("invalid photo burst authority transition")
	}
	for key, post := range pending {
		if post.Kind == "" {
			delete(replay.attachments, key)
		} else {
			replay.attachments[key] = post
		}
	}
	usedDeltas[digest] = true
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return true, nil
}

func validatePhotoBurstGenesis(records []audit.Record) error {
	var sequence, maximum uint64
	for _, record := range records {
		switch record.Kind {
		case auditPhotoBurstSettingsKind, auditPhotoRepresentativeKind:
			if err := validatePhotoBurstRecord(record); err != nil {
				return err
			}
			if record.Kind == auditPhotoBurstSettingsKind {
				sequence, _ = auditUnsignedField(record, "sequence")
			} else {
				rank, _ := auditUnsignedField(record, "rank")
				maximum = max(maximum, rank)
			}
		}
	}
	if maximum > sequence {
		return errors.New("photo representative exceeds sequence at enrollment")
	}
	return nil
}
