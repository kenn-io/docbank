package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

const (
	renderLocalIO   = "redaction_render_local_io"
	renderVerifies  = "masks_and_text"
	renderTimeLimit = 20 * time.Minute
)

var cleanupRedactionRenderStage = (*filepublish.Stage).Cleanup

var renderNativePDFTool = toolDefinition{
	name: "render_native_pdf", title: "Deliver an unnumbered redacted PDF", write: true,
	description: "Save a ZIP with the redacted PDF, sanitized text, and receipt. Supply the vault ID and absolute paths to the preparation, masks, review, and a new destination on the MCP host. " +
		"Checks the delivered PDF against the review before saving. Use stdio for long renders. This call can hold the MCP slot for reviewed PDF and recipient exports for up to 20 minutes.",
	schemas: renderNativePDFSchemas,
}

type renderNativePDFInput struct {
	VaultID         uuid.UUID `json:"vault_id"`
	PreparationPath string    `json:"preparation_path"`
	MasksPath       string    `json:"masks_path"`
	ReviewPath      string    `json:"review_path"`
	DestinationPath string    `json:"destination_path"`
}

type renderNativePDFOutput struct {
	privateCache
	redactionFileOutput

	ReviewSHA256 string `json:"review_sha256"`
	Verification string `json:"verification"`
}

func renderNativePDF(ctx context.Context, lease *daemonLease, raw []byte, logger *slog.Logger) (output renderNativePDFOutput, _ error) {
	var input renderNativePDFInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return output, err
	}
	for _, path := range []string{input.PreparationPath, input.MasksPath, input.ReviewPath, input.DestinationPath} {
		if len(path) > maxPathBytes || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return output, invalidToolArgumentsError()
		}
	}
	if err := validateExportDestination(input.DestinationPath, false); err != nil {
		var invalid *jsonrpc.Error
		if errors.As(err, &invalid) && invalid.Code == jsonrpc.CodeInvalidParams {
			return output, invalidToolArgumentsError()
		}
		return output, renderOutputError(ctx, err)
	}
	if !reviewedBatesActive.CompareAndSwap(false, true) {
		return output, &renderError{code: "redaction_busy", message: "An MCP PDF render or full check is in progress. Retry when it finishes."}
	}
	defer reviewedBatesActive.Store(false)
	ctx, cancel := context.WithTimeout(ctx, renderTimeLimit)
	defer cancel()
	inputs, err := redactionfiles.ReadInputs(ctx, input.PreparationPath, input.MasksPath)
	if err != nil {
		return output, renderInputError(ctx, err)
	}
	review, err := redactionfiles.ReadReview(ctx, input.ReviewPath, inputs)
	if err != nil {
		return output, renderInputError(ctx, err)
	}
	if review.Recipe != pdfproduction.QualifiedRecipe() {
		return output, outdatedRenderReview()
	}

	connection, err := lease.transferConnection(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return output, renderRequestError(ctx, err)
		}
		return output, daemonAcquisitionError(err)
	}
	defer func() { _ = lease.close(connection) }()
	vault, err := connection.API().VaultIdentity(ctx)
	if err != nil {
		return output, renderRequestError(ctx, err)
	}
	if !strings.EqualFold(input.VaultID.String(), vault.VaultID) {
		return output, &renderError{code: "redaction_selection_changed", message: resolutionMessages["redaction_selection_changed"]}
	}

	stage, err := filepublish.CreateStage(filepath.Dir(input.DestinationPath), "docbank-redaction-render-")
	if err != nil {
		return output, renderOutputError(ctx, err)
	}
	defer func() {
		if cleanupErr := cleanupRedactionRenderStage(stage); cleanupErr != nil {
			logger.Warn("MCP redaction ZIP stage cleanup failed", "error", cleanupErr)
			output.CleanupFailed = true
		}
	}()
	archive, contents, err := connection.RenderUnnumberedPDFTo(ctx, inputs, review, stage.File)
	if err != nil {
		return output, renderRequestError(ctx, err)
	}
	if err := checkRenderedNativePDF(ctx, contents, review); err != nil {
		return output, err
	}
	published, err := publishRedactionStage(ctx, stage, input.DestinationPath)
	if !published {
		return output, renderOutputError(ctx, err)
	}
	state := "published"
	if err != nil {
		state = "published_durability_unknown"
		logger.Warn("MCP redaction ZIP saved with uncertain durability", "error", err)
	}
	return renderNativePDFOutput{
		privateCache: newPrivateCache(), ReviewSHA256: review.SHA256, Verification: renderVerifies,
		DestinationPath: input.DestinationPath, Size: int(archive.Size), SHA256: archive.SHA256, State: state,
	}, nil
}

func checkRenderedNativePDF(ctx context.Context, contents daemonconn.RedactionArchiveContents, review processing.RedactionReview) error {
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err := pdfproduction.VerifySaved(verifyCtx, contents.PDF, contents.PDF.Size(), review.Resolved, review.Recipe, contents.Presentation)
	if err != nil {
		return renderVerificationError(ctx, err)
	}
	return nil
}

func renderVerificationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return renderRequestError(ctx, err)
	}
	if _, ok := errors.AsType[*pdfproduction.RSSLimitError](err); ok {
		return &renderError{code: "redaction_memory", message: "The MCP process exceeds the PDF verifier memory ceiling. Retry when it is quieter."}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &renderError{code: "redaction_worker_timeout", message: "The MCP PDF verifier timed out. Try a smaller document."}
	}
	return renderRequestError(ctx, err)
}

type renderError struct {
	code, message string
	cause         error
}

func (e *renderError) Error() string {
	if e.cause != nil {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}

func outdatedRenderReview() *renderError {
	return &renderError{code: "redaction_review_changed", message: "This review uses an older writer. Resolve the masks again before rendering."}
}

func renderInputError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, pdfproduction.ErrVerifierVersion) || errors.Is(err, processing.ErrRedactionReviewChanged) {
		return renderRequestError(ctx, err)
	}
	return invalidToolArgumentsError()
}

func renderOutputError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return renderRequestError(ctx, err)
	}
	return &renderError{code: renderLocalIO, message: "The ZIP could not be saved. Check the destination and available disk space.", cause: err}
}

func renderRequestError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &renderError{code: "redaction_timeout", message: "PDF delivery timed out. Use stdio for long renders."}
	case ctx.Err() != nil:
		return &renderError{code: "redaction_canceled", message: "PDF delivery was canceled."}
	case errors.Is(err, processing.ErrRedactionReviewChanged):
		return &renderError{code: "redaction_review_changed", message: "The review no longer matches the preparation and masks. Resolve them again."}
	case errors.Is(err, daemonconn.ErrIntegrity), errors.Is(err, pdfproduction.ErrPDFIntegrity):
		return &renderError{code: resolutionIntegrity, message: "The delivered ZIP or PDF did not match the review."}
	}
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return renderOutputError(ctx, err)
	}
	code, _ := daemonconn.ProblemCode(err)
	switch code {
	case "redaction_review_changed":
		return &renderError{code: code, message: "The daemon could not use this review. Resolve the masks again before rendering."}
	case "redaction_timeout":
		return &renderError{code: code, message: "The daemon did not finish rendering within its request deadline."}
	case "redaction_worker_timeout":
		return &renderError{code: code, message: "The daemon PDF worker timed out."}
	case "redaction_unavailable":
		return &renderError{code: code, message: "Rendering needs a daemon with the Linux amd64 page runtime configured."}
	case "redaction_limit":
		return &renderError{code: code, message: "The render exceeds the size or work limits."}
	case "redaction_failed":
		return &renderError{code: code, message: "The daemon could not render the PDF. Check its log."}
	case resolutionIntegrity:
		return &renderError{code: code, message: "The daemon could not verify the original or rendered PDF."}
	}
	return resolutionRequestError(ctx, err)
}

func renderNativePDFSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	input := rootObjectSchema(schema{
		schemaVaultIDField: uuidSchema(), "preparation_path": path, "masks_path": path,
		"review_path": path, destinationPathField: path,
	}, schemaVaultIDField, "preparation_path", "masks_path", "review_path", destinationPathField)
	output := rootObjectSchema(withPrivateCache(schema{
		"review_sha256": sha256Schema(), "verification": enumSchema(renderVerifies), "sha256": sha256Schema(),
		destinationPathField: path, schemaSizeField: integerSchema(1, processing.MaxRedactionArchiveBytes),
		schemaStateField: enumSchema("published", "published_durability_unknown"), "cleanup_failed": booleanSchema(),
	}), cacheRequired("review_sha256", "verification", "sha256", destinationPathField, schemaSizeField, schemaStateField, "cleanup_failed")...)
	return input, output
}
