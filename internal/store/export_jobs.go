package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

type ExportClaim struct {
	Job   bundle.Job
	Owner string
	Epoch int64
	Token string
}

func (s *Store) ExportPlanForClaim(ctx context.Context, claim ExportClaim) (bundle.Plan, error) {
	if err := s.CheckExportClaim(ctx, claim); err != nil {
		return bundle.Plan{}, err
	}
	return loadExportPlan(ctx, s.db, claim.Job.PlanID)
}

func loadExportJob(ctx context.Context, q metadataQuerier, id string) (bundle.Job, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT canonical_json FROM export_jobs WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return bundle.Job{}, ErrNotFound
	}
	if err != nil {
		return bundle.Job{}, err
	}
	if len(raw) > bundle.MaxMemberBytes {
		return bundle.Job{}, bundle.ErrLimit
	}
	var job bundle.Job
	err = json.Unmarshal(raw, &job, json.RejectUnknownMembers(true))
	return job, err
}
func putExportJob(ctx context.Context, tx *sql.Tx, j bundle.Job) error {
	raw, err := canonical.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE export_jobs SET canonical_json=?,state=?,expires_at=? WHERE id=?`, raw, j.State, j.ExpiresAt, j.ID)
	return err
}

func (s *Store) ExportJob(ctx context.Context, owner, id string) (bundle.Job, error) {
	var actual string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM export_jobs WHERE id=?`, id).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) || err == nil && actual != owner {
		return bundle.Job{}, ErrNotFound
	}
	if err != nil {
		return bundle.Job{}, err
	}
	j, err := loadExportJob(ctx, s.db, id)
	if err == nil && exportExpired(j.ExpiresAt) {
		err = bundle.ErrExpired
	}
	return j, err
}

func (s *Store) QueueExportJob(ctx context.Context, owner string, r bundle.JobRequest) (bundle.Job, error) {
	if owner == "" || validateUUIDv4(r.OperationID) != nil || validateUUIDv4(r.PlanID) != nil || !canonical.IsSHA256Hex(r.Fingerprint) {
		return bundle.Job{}, bundle.ErrConflict
	}
	raw, err := canonical.Marshal(r)
	if err != nil {
		return bundle.Job{}, err
	}
	digest := pageChecksum(raw)
	var job bundle.Job
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var previous, actual string
		e := tx.QueryRowContext(ctx, `SELECT request_sha256,owner FROM export_jobs WHERE id=?`, r.OperationID).Scan(&previous, &actual)
		if e == nil {
			if actual != owner {
				return ErrNotFound
			}
			if previous != digest {
				return bundle.ErrConflict
			}
			job, e = loadExportJob(ctx, tx, r.OperationID)
			if e == nil && exportExpired(job.ExpiresAt) {
				e = bundle.ErrExpired
			}
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = tx.QueryRowContext(ctx, `SELECT owner FROM export_plans WHERE id=?`, r.PlanID).Scan(&actual); e != nil {
			return ErrNotFound
		}
		if actual != owner {
			return ErrNotFound
		}
		plan, e := loadExportPlan(ctx, tx, r.PlanID)
		if e != nil {
			return e
		}
		if exportExpired(plan.ExpiresAt) {
			return bundle.ErrExpired
		}
		if plan.Fingerprint != r.Fingerprint {
			return bundle.ErrConflict
		}
		var total, owned int
		if e = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN owner=? THEN 1 ELSE 0 END),0) FROM export_jobs`, owner).Scan(&total, &owned); e != nil {
			return e
		}
		if total >= 8 || owned >= 2 {
			return bundle.ErrLimit
		}
		job = bundle.Job{ID: r.OperationID, PlanID: r.PlanID, Fingerprint: r.Fingerprint, State: exportQueuedState, Sequence: 1, CreatedAt: nowRFC3339(), Deadline: exportDeadline(2 * time.Hour)}
		job.ExpiresAt = job.Deadline
		raw, e := canonical.Marshal(job)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO export_jobs(id,owner,plan_id,request_sha256,canonical_json,state,epoch,token,archive_name,expires_at) VALUES(?,?,?,?,?,'queued',0,'','',?)`, job.ID, owner, r.PlanID, digest, raw, job.ExpiresAt)
		if e != nil {
			return e
		}
		return extendExportRetention(ctx, tx, plan.ID, job.ExpiresAt)
	})
	return job, err
}

func extendExportRetention(ctx context.Context, tx *sql.Tx, planID, until string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE export_plans SET expires_at=max(expires_at,?) WHERE id=?`, until, planID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE export_sources SET expires_at=max(expires_at,?) WHERE id=(SELECT source_id FROM export_plans WHERE id=?)`, until, planID)
	return err
}

func (s *Store) ClaimExportJob(ctx context.Context) (ExportClaim, error) {
	var claim ExportClaim
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id,owner FROM export_jobs WHERE state='queued' ORDER BY id LIMIT 1`).Scan(&id, &claim.Owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		claim.Job, err = loadExportJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if exportExpired(claim.Job.Deadline) {
			claim.Job.State = exportFailedState
			claim.Job.Failure = "deadline"
			claim.Job.Sequence++
			return putExportJob(ctx, tx, claim.Job)
		}
		claim.Token = uuid.NewString()
		err = tx.QueryRowContext(ctx, `UPDATE export_jobs SET epoch=epoch+1,token=? WHERE id=? RETURNING epoch`, claim.Token, id).Scan(&claim.Epoch)
		if err != nil {
			return err
		}
		claim.Job.State = exportRunningState
		claim.Job.Attempt = claim.Epoch
		claim.Job.Sequence++
		return putExportJob(ctx, tx, claim.Job)
	})
	if err == nil && claim.Token == "" {
		err = ErrNotFound
	}
	return claim, err
}

func checkExportClaim(ctx context.Context, q metadataQuerier, c ExportClaim) error {
	var valid bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_jobs WHERE id=? AND state='running' AND epoch=? AND token=? AND expires_at>?)`, c.Job.ID, c.Epoch, c.Token, nowRFC3339()).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid || c.Token == "" {
		return bundle.ErrFenced
	}
	return nil
}
func (s *Store) CheckExportClaim(ctx context.Context, c ExportClaim) error {
	return checkExportClaim(ctx, s.db, c)
}

func (s *Store) AdvanceExportJob(ctx context.Context, c ExportClaim, roles int, bytes int64) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := checkExportClaim(ctx, tx, c); err != nil {
			return err
		}
		j, err := loadExportJob(ctx, tx, c.Job.ID)
		if err != nil {
			return err
		}
		if roles < j.CompletedRoles || bytes < j.CompletedBytes || roles > bundle.MaxRoles || bytes > bundle.MaxRoleBytes {
			return bundle.ErrConflict
		}
		j.CompletedRoles = roles
		j.CompletedBytes = bytes
		j.Sequence++
		return putExportJob(ctx, tx, j)
	})
}

func (s *Store) FinishExportJob(ctx context.Context, c ExportClaim, receipt *bundle.Receipt, archiveName, failure string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := checkExportClaim(ctx, tx, c); err != nil {
			return err
		}
		j, err := loadExportJob(ctx, tx, c.Job.ID)
		if err != nil {
			return err
		}
		j.Sequence++
		if receipt == nil {
			j.State = exportFailedState
			j.Failure = failure
			j.ExpiresAt = exportDeadline(10 * time.Minute)
		} else {
			if receipt.Format != bundle.Format || receipt.PlanFingerprint != j.Fingerprint || !canonical.IsSHA256Hex(receipt.SHA256) || receipt.Size < 1 || receipt.Size > bundle.MaxArchiveBytes || archiveName != j.ID+".zip" {
				return bundle.ErrConflict
			}
			p, err := loadExportPlan(ctx, tx, j.PlanID)
			if err != nil {
				return err
			}
			if receipt.Entries != p.RoleEntries+3 || j.CompletedRoles != p.RoleEntries || j.CompletedBytes != p.RoleBytes {
				return bundle.ErrConflict
			}
			j.State = "completed"
			j.Receipt = receipt
			j.ExpiresAt = exportDeadline(24 * time.Hour)
			if err = extendExportRetention(ctx, tx, j.PlanID, j.ExpiresAt); err != nil {
				return err
			}
		}
		if err = putExportJob(ctx, tx, j); err != nil {
			return err
		}
		if receipt == nil {
			if err = reduceExportRetention(ctx, tx, j.PlanID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE export_jobs SET token='',archive_name=? WHERE id=?`, archiveName, j.ID)
		return err
	})
}

func (s *Store) CancelExportJob(ctx context.Context, owner, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT owner FROM export_jobs WHERE id=?`, id).Scan(&actual); err != nil {
			return ErrNotFound
		}
		if actual != owner {
			return ErrNotFound
		}
		j, err := loadExportJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.State == "completed" {
			return bundle.ErrConflict
		}
		if j.State == "canceled" || j.State == exportFailedState {
			return nil
		}
		j.State = "canceled"
		j.Sequence++
		j.ExpiresAt = exportDeadline(10 * time.Minute)
		if err = putExportJob(ctx, tx, j); err != nil {
			return err
		}
		if err = reduceExportRetention(ctx, tx, j.PlanID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE export_jobs SET epoch=epoch+1,token='' WHERE id=?`, id)
		return err
	})
}

func reduceExportRetention(ctx context.Context, tx *sql.Tx, id string) error {
	p, err := loadExportPlan(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE export_plans SET expires_at=max(?,COALESCE((SELECT max(expires_at) FROM export_jobs WHERE plan_id=? AND state IN ('queued','running','completed')),'')) WHERE id=?`, p.ExpiresAt, id, id); err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM export_sources WHERE id=?`, p.Source.ID).Scan(&raw); err != nil {
		return err
	}
	var source bundle.Source
	if err = json.Unmarshal(raw, &source); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE export_sources SET expires_at=max(?,COALESCE((SELECT max(expires_at) FROM export_plans WHERE source_id=?),'')) WHERE id=?`, source.ExpiresAt, p.Source.ID, p.Source.ID)
	return err
}

func (s *Store) RevokeExportOwner(ctx context.Context, owner string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM export_jobs WHERE owner=? AND state IN ('queued','running') LIMIT 8`, owner)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.CancelExportJob(ctx, owner, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RequeueExportJobs(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		ids, err := pageMetadataKeys(ctx, tx, `SELECT id FROM export_jobs WHERE state='running' ORDER BY id LIMIT 8`)
		if err != nil {
			return err
		}
		for _, id := range ids {
			j, err := loadExportJob(ctx, tx, id)
			if err != nil {
				return err
			}
			j.State = exportQueuedState
			j.Sequence++
			if err = putExportJob(ctx, tx, j); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE export_jobs SET epoch=epoch+1,token='' WHERE state='queued'`)
		return err
	})
}

// RevokeAbandonedExportSessions runs once at daemon startup, when browser
// credentials from the preceding process can no longer authorize work.
func (s *Store) RevokeAbandonedExportSessions(ctx context.Context) error {
	owners, err := pageMetadataKeys(ctx, s.db, `SELECT DISTINCT owner FROM export_jobs WHERE owner<>'master' AND state IN ('queued','running') LIMIT 8`)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if err = s.RevokeExportOwner(ctx, owner); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ExportArchiveRetained(ctx context.Context, id string) (bool, error) {
	var retained bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_jobs WHERE id=? AND state='completed')`, id).Scan(&retained)
	return retained, err
}

// ExpiredExportJobs is bounded; the worker checks archive leases before removal.
func (s *Store) ExpiredExportJobs(ctx context.Context) ([]string, error) {
	return pageMetadataKeys(ctx, s.db, `SELECT id FROM export_jobs WHERE expires_at<=? ORDER BY id LIMIT 100`, nowRFC3339())
}
func (s *Store) DeleteExpiredExportJob(ctx context.Context, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM export_jobs WHERE id=? AND expires_at<=?`, id, nowRFC3339())
		return err
	})
}

func (s *Store) CleanupExportAuthority(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		// Operational claims and private files are retired by the worker first.
		if _, err := tx.ExecContext(ctx, `DELETE FROM export_plans WHERE id IN (SELECT p.id FROM export_plans p WHERE p.expires_at<=? AND NOT EXISTS(SELECT 1 FROM export_jobs j WHERE j.plan_id=p.id) ORDER BY p.id LIMIT 100)`, nowRFC3339()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM export_sources WHERE id IN (SELECT s.id FROM export_sources s WHERE s.expires_at<=? AND NOT EXISTS(SELECT 1 FROM export_plans p WHERE p.source_id=s.id) ORDER BY s.id LIMIT 100)`, nowRFC3339())
		return err
	})
}
