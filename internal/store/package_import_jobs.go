package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

// PackageIngestRun recovers the original source collection identity admitted
// with a received package. A worker must never allocate a fresh ingest run on
// lease reclaim.
func (s *Store) PackageIngestRun(ctx context.Context, packageID string) (IngestRun, error) {
	var record metadataIngest
	record.Type = metadataIngestType
	err := s.db.QueryRowContext(ctx, `SELECT i.id,i.started_at,i.source_kind,i.source_desc
		FROM packages p JOIN ingests i ON i.id=p.ingest_id
		WHERE p.package_id=$1 AND p.direction='received'`, packageID).Scan(
		&record.ID, &record.StartedAt, &record.SourceKind, &record.SourceDesc)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestRun{}, ErrNotFound
	}
	if err != nil {
		return IngestRun{}, fmt.Errorf("reading package ingest run: %w", err)
	}
	if record.SourceKind != "package:loadfile" {
		return IngestRun{}, ErrPackageConflict
	}
	if err := validateIngestRecord(record); err != nil {
		return IngestRun{}, fmt.Errorf("invalid package ingest run: %w", err)
	}
	return IngestRun{record: record}, nil
}

type PackageImportJobRequest struct {
	ID            string `json:"id"`
	Owner         string `json:"owner"`
	OperationID   string `json:"operation_id"`
	RequestSHA256 string `json:"request_sha256"`
	PreflightID   string `json:"preflight_id"`
	PackageID     string `json:"package_id"`
	JobJSON       []byte `json:"job_json" format:"byte"`
}

type PackageImportJob struct {
	PackageImportJobRequest

	State          string `json:"state"`
	Epoch          int64  `json:"epoch"`
	Token          string `json:"-"`
	ClaimOwner     string `json:"-"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

const (
	packageJobQueued  = "queued"
	packageJobRunning = "running"
)

func validatePackageImportJobRequest(request PackageImportJobRequest) error {
	if validateUUIDv4(request.ID) != nil || validateUUIDv4(request.OperationID) != nil ||
		validateUUIDv4(request.PreflightID) != nil || validateUUIDv4(request.PackageID) != nil ||
		request.Owner == "" || len(request.Owner) > 256 || !canonical.IsSHA256Hex(request.RequestSHA256) ||
		len(request.JobJSON) < 2 || len(request.JobJSON) > 128<<10 {
		return ErrPackageConflict
	}
	if _, err := canonical.Decode[map[string]any](request.JobJSON); err != nil {
		return ErrPackageConflict
	}
	return nil
}

func loadPackageImportJobTx(ctx context.Context, q metadataQuerier, id string) (PackageImportJob, error) {
	var job PackageImportJob
	err := q.QueryRowContext(ctx, `SELECT id,owner,operation_id,request_sha256,preflight_id,package_id,
		job_json,state,epoch,token,COALESCE(claim_owner,''),COALESCE(lease_expires_at,''),created_at,updated_at
		FROM package_import_jobs WHERE id=$1`, id).Scan(&job.ID, &job.Owner, &job.OperationID,
		&job.RequestSHA256, &job.PreflightID, &job.PackageID, &job.JobJSON, &job.State,
		&job.Epoch, &job.Token, &job.ClaimOwner, &job.LeaseExpiresAt, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageImportJob{}, ErrNotFound
	}
	return job, err
}

func packageImportJobByOperationTx(ctx context.Context, q metadataQuerier, owner, operationID string, postgres bool) (PackageImportJob, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM package_import_jobs WHERE owner=$1 AND operation_id=$2`, storedString(postgres, owner), operationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageImportJob{}, ErrNotFound
	}
	if err != nil {
		return PackageImportJob{}, err
	}
	return loadPackageImportJobTx(ctx, q, id)
}

// AdmitPackageImport publishes a received package, its source collection and
// queued job in one logical transaction. A retry identified by owner and
// operation returns the original job without creating another collection.
func (s *Store) AdmitPackageImport(ctx context.Context, run IngestRun, pkg PackageRequest,
	request PackageImportJobRequest,
) (PackageImportJob, error) {
	if err := validatePackageImportJobRequest(request); err != nil {
		return PackageImportJob{}, err
	}
	if err := validatePackageRequest(pkg); err != nil {
		return PackageImportJob{}, err
	}
	if run.record.SourceKind != "package:loadfile" || pkg.Direction != packageDirectionReceived ||
		pkg.State != "importing" || pkg.SnapshotID != "" || pkg.IngestID != run.ID() ||
		request.PackageID != pkg.PackageID {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		stored, err := packageImportJobByOperationTx(ctx, tx, request.Owner, request.OperationID, s.postgresSchema != "")
		// A racing retry arrives with a freshly allocated package ID, so the
		// package identity is deliberately not part of the replay comparison.
		replayed, found, err := replayReceipt(stored, err, stored.PreflightID == request.PreflightID &&
			stored.RequestSHA256 == request.RequestSHA256 && bytes.Equal(stored.JobJSON, request.JobJSON), ErrPackageConflict)
		if err != nil || found {
			result = replayed
			return err
		}
		var sourceKind, sourceLocator, profile, profileJSON, mapping, mappingJSON, manifest, manifestBlob string
		var blocking bool
		err = tx.QueryRowContext(ctx, `SELECT source_kind,source_locator,profile_sha256,profile_json,
			mapping_sha256,mapping_json,manifest_sha256,
			manifest_blob_sha256,blocking FROM package_preflights
			WHERE preflight_id=$1 AND owner=$2 AND expires_at>$3`,
			request.PreflightID, storedString(s.postgresSchema != "", request.Owner), nowRFC3339()).Scan(&sourceKind, &sourceLocator,
			&profile, &profileJSON, &mapping, &mappingJSON, &manifest, &manifestBlob, &blocking)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPackageConflict
		}
		if err != nil {
			return err
		}
		jobBinding, err := canonical.Decode[map[string]any](request.JobJSON)
		if err != nil || sourceLocator == "" || jobBinding["source_kind"] != sourceKind ||
			jobBinding["source_locator"] != sourceLocator ||
			blocking || profile != pkg.ProfileSHA256 || profileJSON != pkg.ProfileJSON ||
			mapping != pkg.MappingSHA256 || mappingJSON != pkg.MappingJSON ||
			manifest != pkg.ManifestSHA256 || manifestBlob != pkg.ManifestBlobSHA256 {
			return ErrPackageConflict
		}
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=$1)`,
			pkg.ManifestBlobSHA256).Scan(&present); err != nil {
			return err
		}
		if !present {
			return ErrPackageConflict
		}
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM packages WHERE package_id=$1
			UNION ALL SELECT 1 FROM package_import_jobs WHERE package_id=$2 OR id=$3)`,
			pkg.PackageID, pkg.PackageID, request.ID).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return ErrPackageConflict
		}
		if _, err := s.ensureIngestRunTx(ctx, tx, run); err != nil {
			return err
		}
		now := nowRFC3339()
		_, err = tx.ExecContext(ctx, `INSERT INTO packages(
			package_id,snapshot_id,direction,package_name,party_label,profile_sha256,profile_json,
			mapping_sha256,mapping_json,manifest_sha256,manifest_blob_sha256,
			predecessor_package_id,relation,ingest_id,export_plan_id,state,produced_on,created_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, pkg.PackageID, nil, pkg.Direction,
			pkg.PackageName, storedString(s.postgresSchema != "", pkg.PartyLabel), pkg.ProfileSHA256, pkg.ProfileJSON,
			pkg.MappingSHA256, pkg.MappingJSON, pkg.ManifestSHA256, pkg.ManifestBlobSHA256,
			nullableString(pkg.PredecessorPackageID), storedString(s.postgresSchema != "", pkg.Relation), pkg.IngestID, nil,
			pkg.State, storedNullableString(s.postgresSchema != "", pkg.ProducedOn), now)
		if err != nil {
			return err
		}
		for _, volume := range pkg.Volumes {
			_, err = tx.ExecContext(ctx, `INSERT INTO package_volumes(
				package_id,ordinal,volume_name,declared_root,mapped_root,resolved_root_sha256
			) VALUES($1,$2,$3,$4,$5,$6)`, pkg.PackageID, volume.Ordinal, storedString(s.postgresSchema != "", volume.VolumeName),
				storedString(s.postgresSchema != "", volume.DeclaredRoot), storedString(s.postgresSchema != "", volume.MappedRoot), volume.ResolvedRootSHA256)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_import_jobs(
			id,owner,operation_id,request_sha256,preflight_id,package_id,
			state,epoch,token,job_json,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,'queued',0,'',$7,$8,$9)`, request.ID, storedString(s.postgresSchema != "", request.Owner), request.OperationID,
			request.RequestSHA256, request.PreflightID, pkg.PackageID, request.JobJSON, now, now)
		if err != nil {
			return err
		}
		result, err = loadPackageImportJobTx(ctx, tx, request.ID)
		return err
	})
	return result, err
}

func (s *Store) PackageImportJob(ctx context.Context, owner, operationID string) (PackageImportJob, error) {
	if owner == "" || validateUUIDv4(operationID) != nil {
		return PackageImportJob{}, ErrPackageConflict
	}
	return packageImportJobByOperationTx(ctx, s.db, owner, operationID, s.postgresSchema != "")
}

// PackageImportProgress reports import receipts without exposing private source
// paths or the worker lease. Gaps are bounded for status responses.
type PackageImportProgress struct {
	Committed int
	GapCount  int
	Gaps      []string
}

func (s *Store) PackageImportProgress(ctx context.Context, packageID string) (PackageImportProgress, error) {
	if validateUUIDv4(packageID) != nil {
		return PackageImportProgress{}, ErrPackageConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return PackageImportProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Receipt bytes stay unchanged. Keep the canonical gaps-array recognition
	// used by SQLite, and isolate JSON expansion and byte decoding here.
	hasGaps := `instr(CAST(r.receipt_json AS TEXT),'"gaps":[')>0 AND instr(CAST(r.receipt_json AS TEXT),'"gaps":[]')=0`
	gapCount := `json_array_length(CAST(r.receipt_json AS TEXT),'$.gaps')`
	gapRows := `json_each(CAST(r.receipt_json AS TEXT),'$.gaps') j`
	gapValue, recordGap := "CAST(j.value AS TEXT)", "r.record_key"
	if s.postgresSchema != "" {
		hasGaps = "(r.query_json::jsonb ->> 'has_gaps')::boolean"
		gapArray := `r.query_json::jsonb -> 'gaps'`
		gapCount = "jsonb_array_length(" + gapArray + ")"
		gapRows = "jsonb_array_elements_text(" + gapArray + ") WITH ORDINALITY AS j(value,key)"
		gapValue = "decode(j.value,'base64')"
		recordGap = "convert_to(r.record_key,'UTF8')"
	}
	progress := PackageImportProgress{Gaps: []string{}}
	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN r.state='committed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN `+hasGaps+` THEN `+gapCount+`
			WHEN r.state IN ('rejected','skipped') THEN 1 ELSE 0 END),0)
		FROM package_import_receipts r WHERE r.package_id=$1`, packageID).Scan(&progress.Committed, &progress.GapCount)
	if err != nil {
		return PackageImportProgress{}, fmt.Errorf("reading package import progress: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `WITH package_gaps(record_key,gap_ordinal,gap) AS (
		SELECT r.record_key,CAST(j.key AS INTEGER),`+gapValue+`
		FROM package_import_receipts r JOIN `+gapRows+` ON TRUE
		WHERE r.package_id=$1 AND `+hasGaps+`
		UNION ALL
		SELECT r.record_key,0,`+recordGap+` FROM package_import_receipts r
		WHERE r.package_id=$1 AND r.state IN ('rejected','skipped') AND NOT (`+hasGaps+`)
	)
	SELECT gap FROM package_gaps ORDER BY record_key,gap_ordinal LIMIT 100`, packageID)
	if err != nil {
		return PackageImportProgress{}, fmt.Errorf("reading package import gaps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var gap string
		if err := rows.Scan(&gap); err != nil {
			return PackageImportProgress{}, err
		}
		progress.Gaps = append(progress.Gaps, gap)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return PackageImportProgress{}, err
	}
	return progress, tx.Commit()
}

// ClaimPackageImportJob atomically replaces an expired lease with a new epoch.
func (s *Store) ClaimPackageImportJob(ctx context.Context, worker string, lease time.Duration) (PackageImportJob, error) {
	if worker == "" || len(worker) > 256 || lease < time.Second || lease > time.Hour {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		now := nowRFC3339()
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM package_import_jobs
			WHERE state='queued' OR state='running' AND lease_expires_at<=$1
			ORDER BY id LIMIT 1`, now).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		previous, err := loadPackageImportJobTx(ctx, tx, id)
		if err != nil {
			return err
		}
		token, err := newUUIDv4()
		if err != nil {
			return err
		}
		until := time.Now().UTC().Add(lease).Format(timestampLayout)
		update, err := tx.ExecContext(ctx, `UPDATE package_import_jobs SET state='running',
			claim_owner=$1,lease_expires_at=$2,epoch=epoch+1,token=$3,updated_at=$4
			WHERE id=$5 AND state=$6 AND epoch=$7`, storedString(s.postgresSchema != "", worker), until, token, now, id, previous.State, previous.Epoch)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrPackageConflict
		}
		result, err = loadPackageImportJobTx(ctx, tx, id)
		return err
	})
	return result, err
}

func packageImportLeaseHeld(ctx context.Context, tx *sql.Tx, id string, epoch int64, token string) (PackageImportJob, error) {
	job, err := loadPackageImportJobTx(ctx, tx, id)
	if err != nil {
		return PackageImportJob{}, err
	}
	if job.State != packageJobRunning || job.Epoch != epoch || job.Token == "" || job.Token != token ||
		job.LeaseExpiresAt <= nowRFC3339() {
		return PackageImportJob{}, ErrPackageConflict
	}
	return job, nil
}

func (s *Store) RenewPackageImportJob(ctx context.Context, id string, epoch int64, token string, lease time.Duration) (PackageImportJob, error) {
	if validateUUIDv4(id) != nil || lease < time.Second || lease > time.Hour {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := packageImportLeaseHeld(ctx, tx, id, epoch, token); err != nil {
			return err
		}
		until := time.Now().UTC().Add(lease).Format(timestampLayout)
		_, err := tx.ExecContext(ctx, `UPDATE package_import_jobs SET lease_expires_at=$1,updated_at=$2 WHERE id=$3`, until, nowRFC3339(), id)
		if err != nil {
			return err
		}
		result, err = loadPackageImportJobTx(ctx, tx, id)
		return err
	})
	return result, err
}

// ReleasePackageImportJob makes a failed attempt immediately claimable. The
// epoch change fences the abandoned worker even if its old token was retained.
func (s *Store) ReleasePackageImportJob(ctx context.Context, id string, epoch int64, token string) (PackageImportJob, error) {
	if validateUUIDv4(id) != nil {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := packageImportLeaseHeld(ctx, tx, id, epoch, token); err != nil {
			return err
		}
		update, err := tx.ExecContext(ctx, `UPDATE package_import_jobs SET state='queued',epoch=epoch+1,
			claim_owner=NULL,lease_expires_at=NULL,token='',updated_at=$1
			WHERE id=$2 AND state='running' AND epoch=$3 AND token=$4`, nowRFC3339(), id, epoch, token)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrPackageConflict
		}
		result, err = loadPackageImportJobTx(ctx, tx, id)
		return err
	})
	return result, err
}

// FinishPackageImportJob seals the member stream and publishes the package state
// in one transaction under the live lease.
// Terminal failure discards staged files that no committed receipt retains.
func (s *Store) FinishPackageImportJob(ctx context.Context, id string, epoch int64, token, state, snapshotID string, memberStream io.Reader) (PackageImportJob, error) {
	if validateUUIDv4(id) != nil || state != packageStateComplete && state != packageStatePartial && state != packageStateFailed ||
		(state == packageStateComplete || state == packageStatePartial) && (validateUUIDv4(snapshotID) != nil || memberStream == nil) ||
		state == packageStateFailed && (snapshotID != "" || memberStream != nil) {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		job, err := packageImportLeaseHeld(ctx, tx, id, epoch, token)
		if err != nil {
			return err
		}
		pkg, err := loadPackageTx(ctx, tx, job.PackageID)
		if err != nil {
			return err
		}
		if pkg.Direction != packageDirectionReceived || pkg.State != "importing" || pkg.SnapshotID != "" && pkg.SnapshotID != snapshotID {
			return ErrPackageConflict
		}
		if snapshotID != "" {
			snapshot, err := s.sealCollectionSnapshotStreamTx(ctx, tx,
				SnapshotSealHeader{SnapshotID: snapshotID, SourceCollectionIDs: []string{pkg.IngestID}}, memberStream)
			if err != nil {
				return err
			}
			if !slices.Contains(snapshot.SourceCollectionIDs, pkg.IngestID) {
				return ErrPackageConflict
			}
			var members, receipts, matched int
			err = tx.QueryRowContext(ctx, `SELECT
				(SELECT COUNT(*) FROM collection_snapshot_members WHERE snapshot_id=$1),
				(SELECT COUNT(*) FROM package_import_receipts r WHERE r.package_id=$2 AND r.state='committed'),
				(SELECT COUNT(*) FROM collection_snapshot_members m
					JOIN package_import_receipts r ON r.package_id=$3 AND r.occurrence_id=m.occurrence_id
						AND r.content_version_id=m.content_version_id
					WHERE m.snapshot_id=$4)`, snapshotID, pkg.PackageID, pkg.PackageID, snapshotID).Scan(&members, &receipts, &matched)
			if err != nil {
				return err
			}
			if members == 0 || members != receipts || matched != members {
				return ErrPackageConflict
			}
		}
		if state == packageStateFailed {
			if err := s.discardUnreceiptedPackageFilesTx(ctx, tx, pkg); err != nil {
				return err
			}
		}
		now := nowRFC3339()
		_, err = tx.ExecContext(ctx, `UPDATE packages SET state=$1,snapshot_id=COALESCE(snapshot_id,$2),completed_at=$3
			WHERE package_id=$4 AND state='importing'`, state, nullableString(snapshotID), now, pkg.PackageID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE package_import_jobs SET state=$1,epoch=epoch+1,
			claim_owner=NULL,lease_expires_at=NULL,token='',updated_at=$2 WHERE id=$3`, state, now, id)
		if err != nil {
			return err
		}
		result, err = loadPackageImportJobTx(ctx, tx, id)
		return err
	})
	return result, err
}

func (s *Store) CancelPackageImportJob(ctx context.Context, owner, operationID string) (PackageImportJob, error) {
	if owner == "" || validateUUIDv4(operationID) != nil {
		return PackageImportJob{}, ErrPackageConflict
	}
	var result PackageImportJob
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = packageImportJobByOperationTx(ctx, tx, owner, operationID, s.postgresSchema != "")
		if err != nil {
			return err
		}
		if result.State == "cancelled" {
			return nil
		}
		if result.State != packageJobQueued && result.State != packageJobRunning {
			return ErrPackageConflict
		}
		pkg, err := loadPackageTx(ctx, tx, result.PackageID)
		if err != nil {
			return err
		}
		if pkg.State != "importing" {
			return ErrPackageConflict
		}
		if err := s.discardUnreceiptedPackageFilesTx(ctx, tx, pkg); err != nil {
			return err
		}
		now := nowRFC3339()
		_, err = tx.ExecContext(ctx, `UPDATE packages SET state='cancelled',completed_at=$1 WHERE package_id=$2`, now, pkg.PackageID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE package_import_jobs SET state='cancelled',epoch=epoch+1,
			claim_owner=NULL,lease_expires_at=NULL,token='',updated_at=$1 WHERE id=$2`, now, result.ID)
		if err != nil {
			return err
		}
		result, err = loadPackageImportJobTx(ctx, tx, result.ID)
		return err
	})
	return result, err
}

// RevokePackageImportOwner cancels every queued or running import admitted by
// one owner. A revoked browser session must not keep importing its files.
func (s *Store) RevokePackageImportOwner(ctx context.Context, owner string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM package_import_jobs
		WHERE owner=$1 AND state IN ('queued','running')`, storedString(s.postgresSchema != "", owner))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var operations []string
	for rows.Next() {
		var operationID string
		if err = rows.Scan(&operationID); err != nil {
			return err
		}
		operations = append(operations, operationID)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, operationID := range operations {
		if _, err = s.CancelPackageImportJob(ctx, owner, operationID); err != nil && !errors.Is(err, ErrPackageConflict) {
			return err
		}
	}
	return nil
}
