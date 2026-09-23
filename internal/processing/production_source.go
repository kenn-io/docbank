package processing

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionSourceOpener connects the renderer to the catalog-authorized
// finalized rendition and its verified packed blob stream. It has no worker
// registration or API route.
type ProductionSourceOpener struct {
	Catalog *store.Store
	Blobs   *blob.Store
}

func (o ProductionSourceOpener) OpenPinnedProductionPDF(ctx context.Context, job production.Job,
	member documentproduction.PreparedMember) (production.PinnedProductionPDF, error) {
	if o.Catalog == nil || o.Blobs == nil || job.SetID == "" || job.Revision < 1 || member.Member.ID == "" {
		return production.PinnedProductionPDF{}, production.ErrJobConflict
	}
	handle, err := o.Catalog.OpenFinalizedProductionPDF(ctx, job.SetID, job.Revision, member.Member.ID, o.Blobs)
	if err != nil {
		return production.PinnedProductionPDF{}, err
	}
	if handle.SourceVersionID != member.Member.SourceVersionID || handle.PDFSHA256 != member.Member.PDFSHA256 ||
		handle.Size != member.Member.PDFSize || handle.Stream == nil {
		if handle.Stream != nil {
			err = errors.Join(err, handle.Stream.Close())
		}
		return production.PinnedProductionPDF{}, errors.Join(production.ErrJobConflict, err)
	}
	return production.PinnedProductionPDF{PDFSHA256: handle.PDFSHA256, Size: handle.Size, Stream: handle.Stream}, nil
}

var _ production.ProductionPDFSourceOpener = ProductionSourceOpener{}
