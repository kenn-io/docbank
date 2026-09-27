package store

import (
	"context"
	"database/sql"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// ProductionDraftPreviewSource is a read-only, ETag-bound source observation.
// PDF remains untrusted until the preview renderer verifies its full stream.
type ProductionDraftPreviewSource struct {
	Member             documentproduction.PreparedMember
	Recipe             redaction.Recipe
	PreviewInputSHA256 string
	PDF                productionservice.PinnedProductionPDF
}

// CurrentProductionDraftPreviewInput rechecks a retained admission against
// the current draft and evidence heads just before ticket publication.
func (s *Store) CurrentProductionDraftPreviewInput(ctx context.Context,
	command ProductionPreviewCommand) (string, error) {
	if ctx == nil || !validProductionPreviewCommand(command) {
		return "", ErrInvalidProduction
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	input, err := s.selectProductionDraftPreviewInputTx(ctx, tx, command.SetID,
		command.Revision, command.ETag, command.MemberID)
	if err != nil {
		return "", err
	}
	if command.Page > len(input.Member.Resolved.Pages) {
		return "", ErrInvalidProduction
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return input.PreviewInputSHA256, nil
}

// OpenProductionDraftPreviewSource derives one occurrence from the current
// draft and catalog. The caller supplies only identities, never a blob hash.
func (s *Store) OpenProductionDraftPreviewSource(ctx context.Context, setID string,
	revision, etag int64, memberID string, opener RenditionBlobReader) (ProductionDraftPreviewSource, error) {
	if ctx == nil || opener == nil || validateUUIDv4(setID) != nil ||
		validateUUIDv4(memberID) != nil || revision < 1 || etag < 1 {
		return ProductionDraftPreviewSource{}, ErrInvalidProduction
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	defer func() { _ = tx.Rollback() }()
	input, err := s.selectProductionDraftPreviewInputTx(ctx, tx, setID, revision, etag, memberID)
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	stream, size, err := opener.OpenStreamContext(ctx, input.Member.Member.PDFSHA256)
	if err != nil || stream == nil || size != input.Member.Member.PDFSize {
		if stream != nil {
			err = errors.Join(err, stream.Close())
		}
		return ProductionDraftPreviewSource{}, errors.Join(ErrInvalidProduction, err)
	}
	input.PDF = productionservice.PinnedProductionPDF{
		PDFSHA256: input.Member.Member.PDFSHA256, Size: size, Stream: stream,
	}
	return input, nil
}

// selectProductionDraftPreviewInputTx binds one current draft selection and
// the qualified recipe without opening physical PDF bytes.
func (s *Store) selectProductionDraftPreviewInputTx(ctx context.Context, tx *sql.Tx,
	setID string, revision, etag int64, memberID string) (ProductionDraftPreviewSource, error) {
	stored, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	if stored.Draft.State != "draft" || stored.Draft.ETag != etag {
		return ProductionDraftPreviewSource{}, ErrProductionRevisionConflict
	}
	if err := checkProductionEvidenceHeadsTx(ctx, tx, stored.Members); err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	member, found, err := productionservice.PrepareProductionPreviewMember(stored, memberID)
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	if !found {
		return ProductionDraftPreviewSource{}, ErrNotFound
	}
	recipe, err := loadProductionRecipeTx(ctx, tx, stored.Draft)
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	previewRaw, err := canonical.Marshal(struct {
		Draft  redaction.Draft                   `json:"draft"`
		Member documentproduction.PreparedMember `json:"member"`
		Recipe redaction.Recipe                  `json:"recipe"`
	}{stored.Draft, member, recipe})
	if err != nil {
		return ProductionDraftPreviewSource{}, err
	}
	previewSHA := productionSHA256(previewRaw)
	return ProductionDraftPreviewSource{Member: member, Recipe: recipe,
		PreviewInputSHA256: previewSHA}, nil
}
