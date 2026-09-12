package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"slices"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrPageConflict = errors.New("page authority conflicts with a retained result")
	ErrPageFenced   = errors.New("page job claim or selected source is stale")
	ErrPageLimit    = errors.New("page request exceeds limits")
)

// PageBinding is the exact selected-source revision fence used for every read
// and mutation. Historical versions remain explicit; current head is irrelevant.
type PageBinding struct {
	NodeID   int64               `json:"node_id"`
	Revision int64               `json:"revision"`
	Source   document.PageSource `json:"source"`
}

type PageJobRequest struct {
	NodeID             int64               `json:"node_id"`
	Revision           int64               `json:"revision"`
	Source             document.PageSource `json:"source"`
	Pages              []int               `json:"pages"`
	DPI                float64             `json:"dpi"`
	RuntimeFingerprint string              `json:"runtime_fingerprint"`
}

func (r PageJobRequest) Binding() PageBinding {
	return PageBinding{NodeID: r.NodeID, Revision: r.Revision, Source: r.Source}
}

type PageRenderJob struct {
	ID            string                 `json:"id"`
	RequestSHA256 string                 `json:"request_sha256"`
	Request       PageJobRequest         `json:"request"`
	State         string                 `json:"state"`
	Results       []document.PageImageV1 `json:"results"`
	FailureCode   string                 `json:"failure_code"`
	CreatedAt     string                 `json:"created_at"`
	UpdatedAt     string                 `json:"updated_at"`
}

type PageJobClaim struct {
	Job   PageRenderJob
	Epoch int64
	Token string
}

func pageChecksum(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func validatePageRequest(r PageJobRequest) error {
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if r.NodeID < 1 || r.Revision < 1 || r.NodeID > document.MaxPageInteger || r.Revision > document.MaxPageInteger || len(r.Pages) < 1 || len(r.Pages) > document.MaxRenderPages || math.IsNaN(r.DPI) || math.IsInf(r.DPI, 0) || r.DPI < 0 || r.DPI > 1_000_000 || !canonical.IsSHA256Hex(r.RuntimeFingerprint) {
		return ErrPageLimit
	}
	for index, page := range r.Pages {
		if page < 1 || page > document.MaxDocumentPages || index > 0 && r.Pages[index-1] >= page {
			return ErrPageLimit
		}
	}
	return nil
}

func pageSourceTx(ctx context.Context, q metadataQuerier, b PageBinding) error {
	if b.NodeID < 1 || b.Revision < 1 || b.Source.Validate() != nil {
		return ErrPageFenced
	}
	var revision int64
	var trashed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT revision,trashed_at FROM nodes WHERE id=?`, b.NodeID).Scan(&revision, &trashed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision != b.Revision || trashed.Valid {
		return ErrPageFenced
	}
	version, err := scanContentVersion(q.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE node_id=? AND version_id=?`, b.NodeID, b.Source.VersionID))
	if err != nil {
		return err
	}
	if version.BlobHash != b.Source.SHA256 || version.Size != b.Source.Size {
		return ErrPageFenced
	}
	return nil
}

func (s *Store) QueuePageJob(ctx context.Context, id string, r PageJobRequest) (PageRenderJob, error) {
	if validateUUIDv4(id) != nil {
		return PageRenderJob{}, ErrPageConflict
	}
	if err := validatePageRequest(r); err != nil {
		return PageRenderJob{}, err
	}
	request, err := canonical.Marshal(r)
	if err != nil {
		return PageRenderJob{}, err
	}
	digest := pageChecksum(request)
	var job PageRenderJob
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := pageSourceTx(ctx, tx, r.Binding()); err != nil {
			return err
		}
		existing, err := loadPageJob(ctx, tx, `id=?`, id)
		if err == nil {
			if existing.RequestSHA256 != digest {
				return ErrPageConflict
			}
			job = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		existing, err = loadPageJob(ctx, tx, `request_sha256=?`, digest)
		if err == nil {
			job = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM page_render_jobs WHERE state IN ('queued','running')`).Scan(&pending); err != nil {
			return err
		}
		if pending >= 64 {
			return ErrPageLimit
		}
		now := nowRFC3339()
		_, err = tx.ExecContext(ctx, `INSERT INTO page_render_jobs(id,node_id,version_id,request_sha256,request_json,state,results_json,failure_code,epoch,token,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,'',0,'',?,?)`, id, r.NodeID, r.Source.VersionID, digest, request, []byte("[]"), now, now)
		if err != nil {
			return err
		}
		job, err = loadPageJob(ctx, tx, `id=?`, id)
		return err
	})
	return job, err
}

func loadPageJob(ctx context.Context, q metadataQuerier, where string, args ...any) (PageRenderJob, error) {
	var j PageRenderJob
	var request, results []byte
	var nodeID int64
	var versionID string
	err := q.QueryRowContext(ctx, `SELECT id,node_id,version_id,request_sha256,request_json,state,results_json,failure_code,created_at,updated_at FROM page_render_jobs WHERE `+where, args...).Scan(&j.ID, &nodeID, &versionID, &j.RequestSHA256, &request, &j.State, &results, &j.FailureCode, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.Request, err = canonical.Decode[PageJobRequest](request)
	if err != nil {
		return j, err
	}
	j.Results, err = canonical.Decode[[]document.PageImageV1](results)
	if err != nil {
		return j, err
	}
	if pageChecksum(request) != j.RequestSHA256 || j.Request.NodeID != nodeID || j.Request.Source.VersionID != versionID {
		return j, ErrPageConflict
	}
	if err := validatePageJob(j); err != nil {
		return j, err
	}
	return j, nil
}

func validatePageJob(j PageRenderJob) error {
	if validateUUIDv4(j.ID) != nil || !canonical.IsSHA256Hex(j.RequestSHA256) || validateMetadataTime("page job created", j.CreatedAt) != nil || validateMetadataTime("page job updated", j.UpdatedAt) != nil {
		return ErrPageConflict
	}
	if err := validatePageRequest(j.Request); err != nil {
		return err
	}
	b, err := canonical.Marshal(j.Request)
	if err != nil || pageChecksum(b) != j.RequestSHA256 {
		return ErrPageConflict
	}
	switch j.State {
	case "queued", "running", "completed", "canceled", "failed":
	default:
		return ErrPageConflict
	}
	switch j.FailureCode {
	case "", "unavailable", "unsupported", "invalid_output", "stale_source", "interrupted", "storage":
	default:
		return ErrPageConflict
	}
	if (j.State == "failed") != (j.FailureCode != "") {
		return ErrPageConflict
	}
	if len(j.Results) > len(j.Request.Pages) {
		return ErrPageConflict
	}
	var total int64
	for index, r := range j.Results {
		if document.ValidatePageImageV1(r) != nil || r.Source != j.Request.Source || !slices.Contains(j.Request.Pages, r.Page) || index > 0 && j.Results[index-1].Page >= r.Page {
			return ErrPageConflict
		}
		total += r.Size
	}
	if total > document.MaxPageJobBytes || j.State == "completed" && len(j.Results) != len(j.Request.Pages) {
		return ErrPageConflict
	}
	return nil
}

func (s *Store) PageJob(ctx context.Context, id string, binding PageBinding) (PageRenderJob, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PageRenderJob{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := pageSourceTx(ctx, tx, binding); err != nil {
		return PageRenderJob{}, err
	}
	j, err := loadPageJob(ctx, tx, `id=?`, id)
	if err != nil {
		return j, err
	}
	if j.Request.Binding() != binding {
		return j, ErrPageFenced
	}
	return j, tx.Commit()
}

func (s *Store) ClaimPageJob(ctx context.Context) (PageJobClaim, error) {
	var claim PageJobClaim
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := loadPageJob(ctx, tx, `id=(SELECT id FROM page_render_jobs WHERE state='queued' ORDER BY created_at,id LIMIT 1)`)
		if err != nil {
			return err
		}
		claim.Token = uuid.NewString()
		err = tx.QueryRowContext(ctx, `UPDATE page_render_jobs SET state='running',epoch=epoch+1,token=?,updated_at=? WHERE id=? AND state='queued' RETURNING epoch`, claim.Token, nowRFC3339(), job.ID).Scan(&claim.Epoch)
		if err != nil {
			return err
		}
		job.State = "running"
		claim.Job = job
		return nil
	})
	return claim, err
}

func pageClaimTx(ctx context.Context, tx *sql.Tx, claim PageJobClaim) (PageRenderJob, error) {
	if claim.Epoch < 1 || validateUUIDv4(claim.Token) != nil {
		return PageRenderJob{}, ErrPageFenced
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM page_render_jobs WHERE id=? AND epoch=? AND token=? AND state='running')`, claim.Job.ID, claim.Epoch, claim.Token).Scan(&valid); err != nil {
		return PageRenderJob{}, err
	}
	if !valid {
		return PageRenderJob{}, ErrPageFenced
	}
	job, err := loadPageJob(ctx, tx, `id=?`, claim.Job.ID)
	if err != nil {
		return job, err
	}
	if job.RequestSHA256 != claim.Job.RequestSHA256 {
		return job, ErrPageFenced
	}
	if err := pageSourceTx(ctx, tx, job.Request.Binding()); err != nil {
		return job, ErrPageFenced
	}
	return job, nil
}

func (s *Store) CheckPageClaim(ctx context.Context, claim PageJobClaim) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { _, err := pageClaimTx(ctx, tx, claim); return err })
}

func (s *Store) RequeuePageJobs(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE page_render_jobs SET state='queued',epoch=epoch+1,token='',updated_at=? WHERE state='running'`, nowRFC3339())
		return err
	})
}

func (s *Store) CancelPageJob(ctx context.Context, id string, binding PageBinding) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := pageSourceTx(ctx, tx, binding); err != nil {
			return err
		}
		job, err := loadPageJob(ctx, tx, `id=?`, id)
		if err != nil {
			return err
		}
		if job.Request.Binding() != binding {
			return ErrPageFenced
		}
		_, err = tx.ExecContext(ctx, `UPDATE page_render_jobs SET state='canceled',epoch=epoch+1,token='',updated_at=? WHERE id=? AND state IN ('queued','running')`, nowRFC3339(), id)
		return err
	})
}

func (s *Store) FinishPageJob(ctx context.Context, claim PageJobClaim, state, failure string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		// A stale source still permits retiring this exact claim, but never output.
		job, err := loadPageJob(ctx, tx, `id=? AND epoch=? AND token=? AND state='running'`, claim.Job.ID, claim.Epoch, claim.Token)
		if err != nil {
			return ErrPageFenced
		}
		if state != "completed" && state != "failed" {
			return ErrPageConflict
		}
		job.State = state
		job.FailureCode = failure
		if err := validatePageJob(job); err != nil {
			return err
		}
		if state == "completed" {
			if err := pageSourceTx(ctx, tx, job.Request.Binding()); err != nil {
				return ErrPageFenced
			}
			if err := validatePageJobClosure(ctx, tx, job); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE page_render_jobs SET state=?,failure_code=?,token='',updated_at=? WHERE id=?`, state, failure, nowRFC3339(), job.ID)
		return err
	})
}

func appendPageJobResult(ctx context.Context, tx *sql.Tx, job PageRenderJob, image document.PageImageV1) error {
	for _, existing := range job.Results {
		if existing.Page == image.Page {
			if existing != image {
				return ErrPageConflict
			}
			return nil
		}
	}
	job.Results = append(job.Results, image)
	slices.SortFunc(job.Results, func(a, b document.PageImageV1) int { return a.Page - b.Page })
	if err := validatePageJob(job); err != nil {
		return err
	}
	results, err := canonical.Marshal(job.Results)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE page_render_jobs SET results_json=?,updated_at=? WHERE id=?`, results, nowRFC3339(), job.ID)
	return err
}
