package daemonconn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"uuid"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type BatesDownloadResult struct {
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	PageCount    int    `json:"page_count"`
	Verification string `json:"verification"`
}

type batesPDFReader interface {
	io.ReadSeeker
	io.ReaderAt
}

func (c *Connection) BatesExport(ctx context.Context, allocationID string) (api.BatesExport, error) {
	if !validUUIDv4(allocationID) {
		return api.BatesExport{}, errors.New("bates export requires an allocation UUID")
	}
	result, err := c.API().ReadBatesExport(ctx, &apiclient.ReadBatesExportRequestOptions{
		PathParams: &apiclient.ReadBatesExportPath{ID: uuid.MustParse(allocationID)}})
	if err != nil {
		return api.BatesExport{}, err
	}
	if result.AllocationID != allocationID {
		return api.BatesExport{}, integrityErrorf("Bates export allocation differs")
	}
	if _, err := checkedBatesRecipe(*result, nil); err != nil {
		return api.BatesExport{}, err
	}
	return *result, nil
}

func batesArtifact(receipt api.BatesExport) store.BatesArtifact {
	return store.BatesArtifact{ArtifactID: receipt.ArtifactID, AllocationID: receipt.AllocationID,
		BlobSHA256: receipt.BlobSHA256, Size: receipt.Size, PageCount: receipt.PageCount, RecipeSHA256: receipt.RecipeSHA256,
		RecipeJSON: receipt.Recipe, MediaType: receipt.MediaType, ManifestSHA256: receipt.ManifestSHA256,
		State: receipt.State, Pages: receipt.Pages}
}

func checkedBatesRecipe(receipt api.BatesExport, review *processing.RedactionReview) (*processing.NumberedReservation, error) {
	if receipt.Allocation == nil || receipt.Namespace == nil {
		return nil, integrityErrorf("Bates export authority is missing")
	}
	a, n := receipt.Allocation, receipt.Namespace
	allocation := store.BatesAllocation{AllocationID: a.AllocationID, NamespaceID: a.NamespaceID, SnapshotID: a.SnapshotID,
		RequestSHA256: a.RequestSHA256, RecipeSHA256: a.RecipeSHA256, State: a.State, StartSequence: a.StartSequence, EndSequence: a.EndSequence}
	for _, p := range a.Labels {
		allocation.Labels = append(allocation.Labels, store.BatesPageLabel{
			Ordinal: p.Ordinal, OccurrenceID: p.OccurrenceID, SourcePage: p.SourcePage, OutputPage: p.OutputPage, Label: p.Label})
	}
	numbered, err := processing.CheckBatesArtifact(batesArtifact(receipt), allocation, store.BatesNamespace{
		NamespaceID: n.NamespaceID, Prefix: n.Prefix, Suffix: n.Suffix, Padding: n.Padding})
	if err != nil {
		return nil, fmt.Errorf("%w: Bates recipe or allocation binding: %w", ErrIntegrity, err)
	}
	if review != nil {
		if numbered == nil {
			return nil, integrityErrorf("review supplied for an ordinary stamped artifact")
		}
		if err := numbered.Recipe.MatchReview(*review); err != nil {
			return nil, err
		}
	}
	return numbered, nil
}

// DownloadBatesExport returns owned bytes and reports the selected verification scope.
func (c *Connection) DownloadBatesExport(ctx context.Context, receipt api.BatesExport,
	review *processing.RedactionReview,
) ([]byte, BatesDownloadResult, error) {
	check, err := selectBatesVerification(receipt, review)
	if err != nil {
		return nil, BatesDownloadResult{}, err
	}
	var out bytes.Buffer
	if err := c.copyBatesExport(ctx, receipt, &out); err != nil {
		return nil, BatesDownloadResult{}, err
	}
	result, err := verifyBatesDownload(ctx, bytes.NewReader(out.Bytes()), receipt, review, check)
	if err != nil {
		return nil, BatesDownloadResult{}, err
	}
	return out.Bytes(), result, nil
}

// DownloadBatesExportTo checks one chosen contract in the caller's private stage.
// Supplying review context selects full checking before any bytes are downloaded.
func (c *Connection) DownloadBatesExportTo(ctx context.Context, receipt api.BatesExport,
	destination *os.File, review *processing.RedactionReview,
) (BatesDownloadResult, error) {
	check, err := selectBatesVerification(receipt, review)
	if err != nil {
		return BatesDownloadResult{}, err
	}
	if err := destination.Truncate(0); err != nil {
		return BatesDownloadResult{}, err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return BatesDownloadResult{}, err
	}
	if err := c.copyBatesExport(ctx, receipt, destination); err != nil {
		return BatesDownloadResult{}, err
	}
	result, err := verifyBatesDownload(ctx, io.NewSectionReader(destination, 0, receipt.Size), receipt, review, check)
	if err != nil {
		return BatesDownloadResult{}, err
	}
	_, err = destination.Seek(0, io.SeekStart)
	return result, err
}

func (c *Connection) copyBatesExport(ctx context.Context, receipt api.BatesExport,
	destination io.Writer,
) error {
	current, err := c.BatesExport(ctx, receipt.AllocationID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, receipt) {
		return integrityErrorf("Bates export receipt changed")
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).DownloadBatesExportContent(runtime.WithStreamingResponse(ctx),
		&apiclient.DownloadBatesExportContentRequestOptions{PathParams: &apiclient.DownloadBatesExportContentPath{ID: uuid.MustParse(receipt.AllocationID)}})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return decodeError(response)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(destination, hash), redactionReader{ctx, io.LimitReader(response.Body, receipt.Size+1)})
	if err != nil {
		return err
	}
	if n != receipt.Size || hex.EncodeToString(hash.Sum(nil)) != receipt.BlobSHA256 {
		return integrityErrorf("Bates export bytes disagree with recorded receipt")
	}
	return nil
}

func verifyBatesDownload(ctx context.Context, reader batesPDFReader, receipt api.BatesExport,
	review *processing.RedactionReview, check batesVerification,
) (BatesDownloadResult, error) {
	result := BatesDownloadResult{Size: receipt.Size, SHA256: receipt.BlobSHA256, PageCount: receipt.PageCount, Verification: check.scope}
	if check.scope != "stamped_labels" {
		if review != nil {
			presentation, err := check.numbered.Recipe.Presentation()
			if err != nil {
				return BatesDownloadResult{}, err
			}
			if err := pdfproduction.VerifySaved(ctx, reader, receipt.Size, review.Resolved, review.Recipe, presentation); err != nil {
				return BatesDownloadResult{}, err
			}
			result.Verification = "masks_text_and_numbers"
		}
		return result, ctx.Err()
	}
	labels := make([]pdfstamp.PageLabel, len(receipt.Pages))
	for i, p := range receipt.Pages {
		labels[i] = pdfstamp.PageLabel{SourcePage: p.SourcePage, Label: p.Label}
	}
	verified, err := pdfstamp.VerifyStampedReader(ctx, reader, labels)
	if err != nil {
		return BatesDownloadResult{}, fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	if verified.PageCount != receipt.PageCount || verified.Size != receipt.Size || verified.SHA256 != receipt.BlobSHA256 {
		return BatesDownloadResult{}, integrityErrorf("stamped PDF differs from receipt")
	}
	result.Verification = "stamped_labels"
	return result, nil
}

type batesVerification struct {
	numbered *processing.NumberedReservation
	scope    string
}

// Scope is chosen from checked metadata before transfer, never after a failed check.
func selectBatesVerification(receipt api.BatesExport, review *processing.RedactionReview) (batesVerification, error) {
	numbered, err := checkedBatesRecipe(receipt, review)
	if err != nil {
		return batesVerification{}, err
	}
	check := batesVerification{numbered: numbered, scope: "recorded_bytes_and_numbers"}
	if numbered != nil {
		if review != nil {
			check.scope = "masks_text_and_numbers"
		}
	} else {
		var recipe pdfstamp.Recipe
		if err := json.Unmarshal(receipt.Recipe, &recipe); err != nil {
			return check, err
		}
		if recipe.QualifiedEngine() {
			check.scope = "stamped_labels"
		}
	}
	return check, nil
}
