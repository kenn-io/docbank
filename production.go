package docbank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type ProductionMemberPage = api.ProductionMemberPage
type ProductionDecisionPage = api.ProductionDecisionPage
type ProductionSetPage = api.ProductionSetPage
type ProductionMapChunk = store.ProductionMapChunk
type ProductionResolvedMaskPage = store.ProductionResolvedMaskPage
type ProductionFinalizationResult = store.ProductionFinalizationResult
type ProductionJobStatus = store.ProductionJobStatus
type ProductionRecipeCatalog = api.ProductionRecipeCatalog

// ProductionPackageReceipt identifies the exact retained archive streamed by
// an embedded vault. The caller must discard destination bytes on error.
type ProductionPackageReceipt struct {
	JobID          string `json:"job_id"`
	OperationID    string `json:"operation_id"`
	VersionID      string `json:"version_id"`
	ArchiveSHA256  string `json:"archive_sha256"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	Size           int64  `json:"size"`
}

// DownloadProductionPackageTo verifies a retained production package in this
// embedded vault before streaming its recipient archive to destination.
func (v *Vault) DownloadProductionPackageTo(ctx context.Context, jobID, operationID string,
	destination io.Writer) (_ ProductionPackageReceipt, retErr error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPackageReceipt{}, ErrClosed
	}
	if destination == nil {
		return ProductionPackageReceipt{}, errors.New("production package destination is required")
	}
	staged, err := processing.PrepareRetainedProductionPackageDownload(ctx, v.metadata, v.blobs,
		v.vaultRoot, jobID, operationID)
	if err != nil {
		return ProductionPackageReceipt{}, err
	}
	defer func() { retErr = errors.Join(retErr, staged.Close()) }()
	file, err := os.Open(staged.ArchivePath)
	if err != nil {
		return ProductionPackageReceipt{}, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	archive := staged.Retained.Archive.Version
	hasher := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(destination, hasher),
		io.LimitReader(embeddedProductionPackageReader{ctx: ctx, reader: file}, archive.Size+1),
		make([]byte, 256<<10))
	if err != nil {
		return ProductionPackageReceipt{}, err
	}
	if written != archive.Size || hex.EncodeToString(hasher.Sum(nil)) != archive.BlobHash {
		return ProductionPackageReceipt{}, errors.New("production package staged bytes failed verification")
	}
	return ProductionPackageReceipt{JobID: jobID, OperationID: operationID,
		VersionID: archive.ID, ArchiveSHA256: archive.BlobHash,
		EvidenceSHA256: staged.Retained.Evidence.SHA256, Size: archive.Size}, nil
}

type embeddedProductionPackageReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r embeddedProductionPackageReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// ProductionPreviewRequest selects one member page and a replay identity.
type ProductionPreviewRequest struct {
	OperationID string
	MemberID    string
	Page        int
}

// ProductionPreviewBytes contains one verified, unnumbered page preview.
// Text contains only the sanitized output text from the resolved page.
type ProductionPreviewBytes struct {
	PreviewInputSHA256 string
	ResolvedSHA256     string
	Image              []byte
	ImageSHA256        string
	Text               []byte
	TextSHA256         string
}

// ProductionPreview renders an exact current draft selection in this embedded
// vault. Actor is the embedding application's authenticated principal.
func (v *Vault) ProductionPreview(ctx context.Context, actor, setID string,
	revision, etag int64, request ProductionPreviewRequest) (ProductionPreviewBytes, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionPreviewBytes{}, ErrClosed
	}
	command := store.ProductionPreviewCommand{SetID: setID, Revision: revision, ETag: etag,
		OperationID: request.OperationID, MemberID: request.MemberID, Page: request.Page}
	var admitted store.ProductionPreviewAdmission
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		admitted, err = v.metadata.AdmitProductionDraftPreview(ctx, actor, command)
		return err
	})
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	stageDir, err := os.MkdirTemp(v.vaultRoot, ".production-preview-")
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	staged, err := processing.PrepareProductionDraftPreview(ctx, v.metadata, v.blobs,
		stageDir, command.SetID, command.Revision, command.ETag, command.MemberID, command.Page)
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	defer func() { _ = staged.Close() }()
	if staged.PreviewInputSHA256 != admitted.PreviewInputSHA256 {
		return ProductionPreviewBytes{}, store.ErrProductionRevisionConflict
	}
	image, err := readVerifiedProductionPreviewBytes(staged.Image.File, staged.ImageSize, staged.ImageSHA256, 32<<20)
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	text, err := readVerifiedProductionPreviewBytes(staged.Text.File, staged.TextSize, staged.TextSHA256, 16<<20)
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	err = embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		current, err := v.metadata.CurrentProductionDraftPreviewInput(ctx, command)
		if err != nil {
			return err
		}
		if current != admitted.PreviewInputSHA256 {
			return store.ErrProductionRevisionConflict
		}
		return nil
	})
	if err != nil {
		return ProductionPreviewBytes{}, err
	}
	return ProductionPreviewBytes{PreviewInputSHA256: admitted.PreviewInputSHA256,
		ResolvedSHA256: staged.ResolvedSHA256, Image: image, ImageSHA256: staged.ImageSHA256,
		Text: text, TextSHA256: staged.TextSHA256}, nil
}

func readVerifiedProductionPreviewBytes(file *os.File, size int64, digest string, maxSize int64) ([]byte, error) {
	if size < 0 || size > maxSize {
		return nil, errors.New("production preview exceeds embedded response limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("production preview staged bytes failed verification")
	}
	return data, nil
}

// The exclusive vault lock is held before this startup sweep runs. Interrupted
// embedded previews and downloads leave only private, disposable stages.
func sweepEmbeddedProductionStages(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && (strings.HasPrefix(entry.Name(), ".production-preview-") ||
			strings.HasPrefix(entry.Name(), ".production-download-")) {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *Vault) ProductionRecipes(ctx context.Context) (ProductionRecipeCatalog, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionRecipeCatalog{}, ErrClosed
	}
	return api.QualifiedProductionRecipes()
}

// CreateProductionSet creates an idempotent first draft in this embedded vault.
// Actor is the embedding application's authenticated principal.
func (v *Vault) CreateProductionSet(ctx context.Context, actor string, request redaction.CreateRequest) (redaction.Set, redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, redaction.Draft{}, ErrClosed
	}
	var set redaction.Set
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		set, draft, err = v.metadata.CreateProductionSet(ctx, actor, request)
		return err
	})
	return set, draft, err
}

func (v *Vault) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Set{}, ErrClosed
	}
	return v.metadata.ProductionSet(ctx, setID)
}

func (v *Vault) ProductionSets(ctx context.Context, cursor string, limit int) (ProductionSetPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionSetPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ListProductionSets(ctx, cursor, limit)
	if err != nil {
		return ProductionSetPage{}, err
	}
	return ProductionSetPage{Items: items, NextCursor: next}, nil
}

func (v *Vault) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	return v.metadata.ProductionDraft(ctx, setID, revision)
}

// ForkProductionDraft copies one retained revision into a new editable draft.
// Review declarations and the membership seal are reset by the Store.
func (v *Vault) ForkProductionDraft(ctx context.Context, actor, setID string, revision int64,
	operationID string) (redaction.Draft, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Draft{}, ErrClosed
	}
	var draft redaction.Draft
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		draft, err = v.metadata.ForkProductionDraft(ctx, actor, setID, revision, operationID)
		return err
	})
	return draft, err
}

func (v *Vault) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionMemberPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionMemberPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionMembers(ctx, setID, revision, cursor, limit)
	if err != nil {
		return ProductionMemberPage{}, err
	}
	page := ProductionMemberPage{Items: make([]api.ProductionMember, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionMember(item)
	}
	return page, nil
}

func (v *Vault) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) (ProductionDecisionPage, error) {
	return v.ProductionDecisionsFiltered(ctx, setID, revision, cursor, limit, nil)
}

// ProductionDecisionsFiltered pages only decisions matching an optional
// uncertainty state. Filtered cursors cannot be reused for another view.
func (v *Vault) ProductionDecisionsFiltered(ctx context.Context, setID string, revision int64,
	cursor string, limit int, uncertain *bool) (ProductionDecisionPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionDecisionPage{}, ErrClosed
	}
	if limit == 0 {
		limit = 100
	}
	items, next, err := v.metadata.ProductionDecisionsFiltered(ctx, setID, revision, cursor, limit, uncertain)
	if err != nil {
		return ProductionDecisionPage{}, err
	}
	page := ProductionDecisionPage{Items: make([]api.ProductionDecision, len(items)), NextCursor: next}
	for i, item := range items {
		page.Items[i] = api.ProductionDecision(item)
	}
	return page, nil
}

// ProductionMapChunk reads one bounded page of a retained member map from
// this embedded vault. The caller verifies the assembled map digest.
func (v *Vault) ProductionMapChunk(ctx context.Context, setID string, revision int64, memberID, cursor string, limit int) (ProductionMapChunk, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionMapChunk{}, ErrClosed
	}
	if limit == 0 {
		limit = store.MaxProductionMapChunkBytes
	}
	return v.metadata.ProductionMapChunk(ctx, setID, revision, memberID, cursor, limit)
}

// ResolveProductionSelection reads one ETag-pinned page of a member's final
// pixel mask and the binding needed to review its complete current plan.
func (v *Vault) ResolveProductionSelection(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionResolveRequest) (ProductionResolvedMaskPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionResolvedMaskPage{}, ErrClosed
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	return v.metadata.ProductionResolvedMaskPage(ctx, setID, revision, request.MemberID,
		etag, request.Page, request.Cursor, limit)
}

// FinalizeProductionDraft gates and locks one reviewed revision in this
// embedded vault. It does not reserve or allocate production numbers.
func (v *Vault) FinalizeProductionDraft(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionFinalizeRequest) (ProductionFinalizationResult, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionFinalizationResult{}, ErrClosed
	}
	var result ProductionFinalizationResult
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		result, err = v.metadata.FinalizeProductionDraft(ctx, actor, store.ProductionFinalizeCommand{
			SetID: setID, Revision: revision, ETag: etag, OperationID: request.OperationID,
			NamespaceID: request.NamespaceID, SnapshotID: request.SnapshotID, StartAt: request.StartAt})
		return err
	})
	return result, err
}

func (v *Vault) ProductionJobStatus(ctx context.Context, setID, jobID string) (ProductionJobStatus, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionJobStatus{}, ErrClosed
	}
	return v.metadata.ProductionJobStatus(ctx, setID, jobID)
}

// AdmitProductionJob pins a finalized revision and enqueues one replay-safe job.
func (v *Vault) AdmitProductionJob(ctx context.Context, setID string, revision, etag int64,
	request api.ProductionJobAdmissionRequest) (ProductionJobStatus, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return ProductionJobStatus{}, ErrClosed
	}
	var status ProductionJobStatus
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		if _, err := v.metadata.AdmitFinalizedProductionJob(ctx, setID, revision, etag,
			request.JobID, request.OperationID); err != nil {
			return err
		}
		var err error
		status, err = v.metadata.ProductionJobStatus(ctx, setID, request.JobID)
		return err
	})
	return status, err
}

// CancelProductionJob records a replay-safe cancellation in this embedded vault.
func (v *Vault) CancelProductionJob(ctx context.Context, actor, setID, jobID string, etag int64,
	request api.ProductionJobCancelRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.CancelProductionJobOperation(ctx, actor, setID, jobID, etag, request.OperationID)
		return err
	})
	return receipt, err
}

func (v *Vault) EditProductionInstructions(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionInstructionsRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.EditProductionInstructions(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ApplyProductionChanges(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionChangesRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ApplyProductionChanges(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) AppendProductionMembers(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionMemberAppendRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.AppendProductionMembers(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) SealProductionMembership(ctx context.Context, actor, setID string, revision, etag int64,
	request api.ProductionMembershipSealRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.SealProductionMembership(ctx, actor, setID, revision, request.Domain(etag))
		return err
	})
	return receipt, err
}

func (v *Vault) ReviewProductionMember(ctx context.Context, actor, setID string, revision, etag int64,
	memberID string, request api.ProductionMemberReviewRequest) (redaction.Receipt, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return redaction.Receipt{}, ErrClosed
	}
	var receipt redaction.Receipt
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		receipt, err = v.metadata.ReviewProductionMember(ctx, actor, setID, revision, request.Domain(etag, memberID))
		return err
	})
	return receipt, err
}
