package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"go.kenn.io/docbank/internal/audit"
)

const (
	auditDerivativePurgeSuppressionKind = "derivative_purge_suppression"
	// A profile-independent scope protects an explicitly purged immutable build,
	// including one that was never attached to a processing profile.
	derivativeBuildSuppressionProfile = "82ec12973fbb63f07fc8996d46be5bb922672fa7d3cc26fa2735b3a1fd3389ef"
)

// DerivativeRebuildAuthorization explicitly supersedes one exact durable purge
// suppression. Startup discovery never performs this authority transition.
type DerivativeRebuildAuthorization struct {
	SourceSHA256 string
	// ProfileFingerprint is empty when authorizing the profile-independent exact
	// immutable build scope, including a build that was never attached.
	ProfileFingerprint string
	// ContentVersionID identifies the exact attachment scope when
	// ProfileFingerprint is set. It is empty only for a build-wide scope.
	ContentVersionID   string
	PurgedBuildID      string
	SupersedingBuildID string
	AuthorizedAt       string
}

type derivativePurgeSuppression struct {
	sourceSHA256, profileFingerprint, buildID, purgedAt string
	active                                              bool
	supersededAt, supersedingBuildID                    string
}

func (s *Store) AuthorizeDerivativeRebuild(
	ctx context.Context, authorization DerivativeRebuildAuthorization,
) error {
	profileFingerprint := authorization.ProfileFingerprint
	if profileFingerprint == "" {
		if authorization.ContentVersionID != "" {
			return errors.New("derivative rebuild content version requires profile fingerprint")
		}
		profileFingerprint = derivativeBuildSuppressionProfile
	} else {
		if err := validateUUIDv4(authorization.ContentVersionID); err != nil {
			return fmt.Errorf("derivative rebuild content version: %w", err)
		}
		profileFingerprint = derivativeAttachmentSuppressionScope(
			authorization.ContentVersionID, profileFingerprint)
	}
	for name, value := range map[string]string{
		"source SHA-256":       authorization.SourceSHA256,
		"profile fingerprint":  profileFingerprint,
		"purged build ID":      authorization.PurgedBuildID,
		"superseding build ID": authorization.SupersedingBuildID,
	} {
		if err := validateCatalogSHA256(value, name); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("derivative rebuild authorized_at", authorization.AuthorizedAt); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		prior, err := loadDerivativePurgeSuppressionTx(ctx, tx, authorization.SourceSHA256,
			profileFingerprint, authorization.PurgedBuildID)
		if err != nil {
			return fmt.Errorf("authorizing derivative rebuild: %w", err)
		}
		resulting := prior
		resulting.active = false
		resulting.supersededAt = authorization.AuthorizedAt
		resulting.supersedingBuildID = authorization.SupersedingBuildID
		if !prior.active {
			if prior == resulting {
				return nil
			}
			return errors.New("derivative purge suppression was already superseded differently")
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE derivative_purge_suppressions
			SET active=0,superseded_at=?,superseding_build_id=?
			WHERE source_sha256=? AND profile_fingerprint=? AND build_id=? AND active=1`,
			authorization.AuthorizedAt, authorization.SupersedingBuildID,
			authorization.SourceSHA256, profileFingerprint,
			authorization.PurgedBuildID)
		if err != nil {
			return fmt.Errorf("superseding derivative purge suppression: %w", err)
		}
		count, err := rowsAffectedInt(result)
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("derivative purge suppression changed concurrently")
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		change, err := derivativeSuppressionAuditChange(prior, resulting)
		if err != nil {
			return err
		}
		return s.persistAuditedDerivativeSuppressionChanges(ctx, tx, []audit.Record{change})
	})
}

func installDerivativePurgeSuppressionsTx(
	ctx context.Context, tx *sql.Tx, buildScopeIDs map[string]struct{},
	attachments []purgeAttachment, purgedAt string,
) ([]audit.Record, error) {
	scopesByBuild := make(map[string]map[string]struct{})
	scopeBuildIDs := make(map[string]struct{}, len(buildScopeIDs)+len(attachments))
	for buildID := range buildScopeIDs {
		scopeBuildIDs[buildID] = struct{}{}
	}
	for _, attachment := range attachments {
		scopeBuildIDs[attachment.buildID] = struct{}{}
		scopes := scopesByBuild[attachment.buildID]
		if scopes == nil {
			scopes = make(map[string]struct{})
			scopesByBuild[attachment.buildID] = scopes
		}
		scopes[derivativeAttachmentSuppressionScope(
			attachment.contentVersionID, attachment.profileFingerprint)] = struct{}{}
	}
	var scopes []derivativePurgeSuppression
	for _, buildID := range derivativeSortedKeys(scopeBuildIDs) {
		_, addBuildScope := buildScopeIDs[buildID]
		buildScopes, err := loadDerivativePurgeSuppressionScopesTx(
			ctx, tx, buildID, scopesByBuild[buildID], addBuildScope, purgedAt)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, buildScopes...)
	}
	return installDerivativePurgeSuppressionRecordsTx(ctx, tx, scopes)
}

func installDerivativePurgeSuppressionRecordsTx(
	ctx context.Context, tx *sql.Tx, scopes []derivativePurgeSuppression,
) ([]audit.Record, error) {
	var changes []audit.Record
	for _, resulting := range scopes {
		prior, err := loadDerivativePurgeSuppressionTx(ctx, tx,
			resulting.sourceSHA256, resulting.profileFingerprint, resulting.buildID)
		switch {
		case errors.Is(err, ErrNotFound):
			prior = derivativePurgeSuppression{}
		case err != nil:
			return nil, err
		case prior.active:
			continue
		}
		_, err = tx.ExecContext(ctx, `
				INSERT INTO derivative_purge_suppressions(
					source_sha256,profile_fingerprint,build_id,purged_at,active,
					superseded_at,superseding_build_id
				) VALUES(?,?,?,?,1,NULL,NULL)
				ON CONFLICT(source_sha256,profile_fingerprint,build_id) DO UPDATE SET
					purged_at=excluded.purged_at,active=1,
					superseded_at=NULL,superseding_build_id=NULL`,
			resulting.sourceSHA256, resulting.profileFingerprint,
			resulting.buildID, resulting.purgedAt)
		if err != nil {
			return nil, fmt.Errorf("recording derivative purge suppression: %w", err)
		}
		change, err := derivativeSuppressionAuditChange(prior, resulting)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		left, _ := audit.Encode(changes[i])
		right, _ := audit.Encode(changes[j])
		return string(left) < string(right)
	})
	return changes, nil
}

func loadDerivativePurgeSuppressionScopesTx(
	ctx context.Context, tx *sql.Tx, buildID string,
	profileFingerprints map[string]struct{}, addBuildScope bool, purgedAt string,
) ([]derivativePurgeSuppression, error) {
	var sourceSHA256 string
	if err := tx.QueryRowContext(ctx,
		`SELECT source_sha256 FROM rendition_builds WHERE build_id=?`, buildID,
	).Scan(&sourceSHA256); err != nil {
		return nil, fmt.Errorf("reading derivative purge suppression scope: %w", err)
	}
	profiles := make(map[string]struct{}, len(profileFingerprints)+1)
	if addBuildScope {
		profiles[derivativeBuildSuppressionProfile] = struct{}{}
	}
	for profileFingerprint := range profileFingerprints {
		profiles[profileFingerprint] = struct{}{}
	}
	scopes := make([]derivativePurgeSuppression, 0, len(profiles))
	for _, profileFingerprint := range derivativeSortedKeys(profiles) {
		scopes = append(scopes, derivativePurgeSuppression{
			sourceSHA256: sourceSHA256, profileFingerprint: profileFingerprint,
			buildID: buildID, purgedAt: purgedAt, active: true,
		})
	}
	return scopes, nil
}

func loadDerivativePurgeSuppressionTx(
	ctx context.Context, tx metadataQuerier, sourceSHA256, profileFingerprint, buildID string,
) (derivativePurgeSuppression, error) {
	record := derivativePurgeSuppression{
		sourceSHA256: sourceSHA256, profileFingerprint: profileFingerprint, buildID: buildID,
	}
	var supersededAt, supersedingBuildID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT purged_at,active,superseded_at,superseding_build_id
		FROM derivative_purge_suppressions
		WHERE source_sha256=? AND profile_fingerprint=? AND build_id=?`,
		sourceSHA256, profileFingerprint, buildID).Scan(
		&record.purgedAt, &record.active, &supersededAt, &supersedingBuildID)
	if errors.Is(err, sql.ErrNoRows) {
		return derivativePurgeSuppression{}, ErrNotFound
	}
	if err != nil {
		return derivativePurgeSuppression{}, err
	}
	if supersededAt.Valid {
		record.supersededAt = supersededAt.String
	}
	if supersedingBuildID.Valid {
		record.supersedingBuildID = supersedingBuildID.String
	}
	return record, nil
}

func legacyDerivativeSuppressedTx(
	ctx context.Context, tx metadataQuerier, sourceSHA256, contentVersionID, profileFingerprint string,
) (bool, error) {
	var suppressed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM derivative_purge_suppressions
		WHERE source_sha256=? AND profile_fingerprint=? AND active=1
	)`, sourceSHA256, derivativeAttachmentSuppressionScope(
		contentVersionID, profileFingerprint)).Scan(&suppressed)
	return suppressed, err
}

func derivativeBuildSuppressedTx(
	ctx context.Context, tx metadataQuerier, sourceSHA256, buildID string,
) (bool, error) {
	var suppressed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM derivative_purge_suppressions
		WHERE source_sha256=? AND profile_fingerprint=? AND build_id=? AND active=1
	)`, sourceSHA256, derivativeBuildSuppressionProfile, buildID).Scan(&suppressed)
	return suppressed, err
}

func derivativeAttachmentSuppressedTx(
	ctx context.Context, tx metadataQuerier, sourceSHA256, contentVersionID,
	profileFingerprint, buildID string,
) (bool, error) {
	var suppressed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM derivative_purge_suppressions
		WHERE source_sha256=? AND build_id=? AND active=1
		  AND profile_fingerprint IN (?,?)
	)`, sourceSHA256, buildID, derivativeAttachmentSuppressionScope(
		contentVersionID, profileFingerprint), derivativeBuildSuppressionProfile).Scan(&suppressed)
	return suppressed, err
}

func derivativeAttachmentSuppressionScope(contentVersionID, profileFingerprint string) string {
	digest := sha256.Sum256([]byte("docbank:derivative-attachment-suppression:v1\x00" +
		contentVersionID + "\x00" + profileFingerprint))
	return hex.EncodeToString(digest[:])
}

func derivativeSuppressionAuditRecord(
	record derivativePurgeSuppression,
) (audit.Record, error) {
	digests := make([]audit.Value, 3)
	for index, value := range []string{record.sourceSHA256, record.profileFingerprint, record.buildID} {
		digest, err := audit.DigestHex(value)
		if err != nil {
			return audit.Record{}, err
		}
		digests[index] = digest
	}
	purgedAt, err := audit.Timestamp(record.purgedAt)
	if err != nil {
		return audit.Record{}, err
	}
	supersededAt, supersedingBuildID := audit.Absent(), audit.Absent()
	if record.supersededAt != "" {
		supersededAt, err = audit.Timestamp(record.supersededAt)
		if err != nil {
			return audit.Record{}, err
		}
	}
	if record.supersedingBuildID != "" {
		supersedingBuildID, err = audit.DigestHex(record.supersedingBuildID)
		if err != nil {
			return audit.Record{}, err
		}
	}
	return audit.Record{Kind: auditDerivativePurgeSuppressionKind, Fields: []audit.Field{
		{Name: "source_sha256", Value: digests[0]},
		{Name: "profile_fingerprint", Value: digests[1]},
		{Name: "build_id", Value: digests[2]},
		{Name: "purged_at", Value: purgedAt},
		{Name: "active", Value: audit.Bool(record.active)},
		{Name: "superseded_at", Value: supersededAt},
		{Name: "superseding_build_id", Value: supersedingBuildID},
	}}, nil
}

func derivativeSuppressionAuditChange(
	prior, resulting derivativePurgeSuppression,
) (audit.Record, error) {
	var pre audit.Record
	if prior.sourceSHA256 != "" {
		var err error
		pre, err = derivativeSuppressionAuditRecord(prior)
		if err != nil {
			return audit.Record{}, err
		}
	}
	post, err := derivativeSuppressionAuditRecord(resulting)
	if err != nil {
		return audit.Record{}, err
	}
	return makeAttachedMetadataChange(pre, post)
}

func (s *Store) persistAuditedDerivativeSuppressionChanges(
	ctx context.Context, tx *sql.Tx, changes []audit.Record,
) error {
	authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating audited derivative-purge operation: %w", err)
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, authority.lineageID, operationID, nowRFC3339())
	if err != nil {
		return err
	}
	delta, digest, err := makeAttachedMetadataDelta(values.operationID, changes)
	if err != nil {
		return err
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, audit.Absent())
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(
		allocation, uint64(len(changes)), digest.value)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func appendAuditDerivativePurgeSuppressions(
	ctx context.Context, tx metadataQuerier, records *[]audit.Record,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_sha256,profile_fingerprint,build_id,purged_at,active,
		       COALESCE(superseded_at,''),COALESCE(superseding_build_id,'')
		FROM derivative_purge_suppressions
		ORDER BY source_sha256,profile_fingerprint,build_id`)
	if err != nil {
		return fmt.Errorf("reading audit derivative purge suppressions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var suppression derivativePurgeSuppression
		if err := rows.Scan(&suppression.sourceSHA256, &suppression.profileFingerprint,
			&suppression.buildID, &suppression.purgedAt, &suppression.active,
			&suppression.supersededAt, &suppression.supersedingBuildID); err != nil {
			return fmt.Errorf("scanning audit derivative purge suppression: %w", err)
		}
		record, err := derivativeSuppressionAuditRecord(suppression)
		if err != nil {
			return err
		}
		*records = append(*records, record)
	}
	return rowsError("audit derivative purge suppressions", rows)
}

func (replay *auditedHistoryReplay) applyUnscopedDerivativeSuppressionChanges(
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
	if err != nil || kind != auditDerivativePurgeSuppressionKind {
		return false, err
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return true, err
	}
	if err := requireAuditUnsigned(allocation.record,
		auditAttachedMetadataChangeCountField, uint64(len(changes))); err != nil {
		return true, err
	}
	for _, change := range changes {
		if err := requireAuditText(change, "record_kind", auditDerivativePurgeSuppressionKind); err != nil {
			return true, err
		}
		pre, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
		if err != nil {
			return true, err
		}
		post, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
		if err != nil || !hasPost {
			return true, errors.New("derivative purge suppression change lacks post-state")
		}
		if err := validateReplayedDerivativeSuppression(post); err != nil {
			return true, err
		}
		identity, err := attachedAuditIdentity(post)
		if err != nil {
			return true, err
		}
		storedIdentity, err := auditNestedField(change, "stable_identity")
		if err != nil || !auditRecordEqual(storedIdentity, identity) {
			return true, errors.New("derivative purge suppression identity does not match its record")
		}
		key, err := attachedAuditKey(post)
		if err != nil {
			return true, err
		}
		current, exists := replay.attachments[key]
		if hasPre {
			if err := validateReplayedDerivativeSuppression(pre); err != nil {
				return true, err
			}
			if !exists || !auditRecordEqual(current, pre) {
				return true, errors.New("derivative purge suppression pre-state does not match replay")
			}
		} else if exists {
			return true, errors.New("derivative purge suppression addition reuses authority")
		}
		if hasPre && auditRecordEqual(pre, post) {
			return true, errors.New("derivative purge suppression transition changes no authority")
		}
		replay.attachments[key] = post
	}
	usedDeltas[digest] = true
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return true, nil
}

func validateReplayedDerivativeSuppression(record audit.Record) error {
	if record.Kind != auditDerivativePurgeSuppressionKind {
		return errors.New("invalid derivative purge suppression kind")
	}
	for _, name := range []string{"source_sha256", "profile_fingerprint", "build_id"} {
		if _, err := auditDigestField(record, name); err != nil {
			return err
		}
	}
	if _, err := auditTimestampField(record, "purged_at"); err != nil {
		return err
	}
	active, err := auditBoolField(record, "active")
	if err != nil {
		return err
	}
	supersededAt, err := auditOptionalTimestampField(record, "superseded_at")
	if err != nil {
		return err
	}
	superseding, err := auditOptionalDigestField(record, "superseding_build_id")
	if err != nil {
		return err
	}
	if active == (supersededAt != nil || superseding != nil) ||
		(supersededAt == nil) != (superseding == nil) {
		return errors.New("derivative purge suppression active state is inconsistent")
	}
	return nil
}
