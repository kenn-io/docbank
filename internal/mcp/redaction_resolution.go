package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactionfiles"
)

const (
	resolutionIntegrity = "redaction_integrity"
	resolutionLocalIO   = "redaction_resolution_local_io"
	expansionRequired   = "selection_expansion_required"
)

var resolveNativePDFTool = toolDefinition{
	name: "resolve_native_pdf", title: "Resolve PDF masks", write: true,
	description: "Save a private review or full mask expansion proposal using the daemon's PDF resolver. " +
		"Supply the vault ID, absolute preparation and masks paths, and a new absolute destination on the MCP host. " +
		"If expansion is required, inspect the proposal, edit the masks explicitly, and resolve to a new file.",
	schemas: resolveNativePDFSchemas,
}

type resolveNativePDFInput struct {
	VaultID         uuid.UUID `json:"vault_id"`
	PreparationPath string    `json:"preparation_path"`
	MasksPath       string    `json:"masks_path"`
	DestinationPath string    `json:"destination_path"`
}

type resolveNativePDFOutput struct {
	privateCache
	redactionFileOutput

	Outcome      string `json:"outcome"`
	ReviewSHA256 string `json:"review_sha256,omitzero"`
	NextStep     string `json:"next_step,omitzero"`
}

type resolutionError struct {
	code  string
	cause error
}

func (e *resolutionError) Error() string {
	if e.cause != nil {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}

var resolutionMessages = map[string]string{
	notFoundCode:                    "The selected retained PDF is unavailable.",
	"source_stale":                  "Prepare and review the source again.",
	"redaction_preparation_changed": "The PDF evidence changed. Prepare and review it again.",
	"redaction_selection_changed":   "Select the vault that contains the prepared PDF.",
	"redaction_busy":                "Another native PDF operation is active in the daemon. Retry when it finishes.",
	"redaction_memory":              "The daemon exceeds the renderer memory ceiling. Retry when it is quieter.",
	"mapping_incomplete":            "Cover ordered gaps with masks and unordered gaps with whole-page masks.",
	"redaction_canceled":            "PDF resolution was canceled.",
	"redaction_timeout":             "PDF resolution exceeded its request deadline.",
	"redaction_worker_timeout":      "The daemon PDF worker exceeded its inspection deadline.",
	"redaction_unavailable":         "Resolution needs a daemon with the Linux amd64 page runtime configured.",
	"redaction_unsupported":         "The selected PDF does not support native review.",
	"redaction_limit":               "Resolution exceeds the size or work limits. Review a smaller selection or use a whole-page mask.",
	"redaction_failed":              "The daemon could not resolve the masks. Check its log.",
	"redaction_verifier_version":    "The preparation or review needs a different verifier version.",
	"invalid_decision":              "Check the mask selectors against the preparation.",
	"decision_conflict":             "The masks conflict. Review the selectors and try again.",
	resolutionIntegrity:             "The review or expansion proposal did not match the submitted inputs.",
	resolutionLocalIO:               "The resolution file could not be saved. Check the destination and available disk space.",
}

func resolveNativePDF(ctx context.Context, lease *daemonLease, raw []byte, logger *slog.Logger) (resolveNativePDFOutput, error) {
	var input resolveNativePDFInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return resolveNativePDFOutput{}, err
	}
	for _, path := range []string{input.PreparationPath, input.MasksPath, input.DestinationPath} {
		if len(path) > maxPathBytes || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return resolveNativePDFOutput{}, invalidToolArgumentsError()
		}
	}
	if err := validateExportDestination(input.DestinationPath, false); err != nil {
		return resolveNativePDFOutput{}, resolutionDestinationError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := redactionfiles.ReadInputs(ctx, input.PreparationPath, input.MasksPath)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, pdfproduction.ErrVerifierVersion) {
			return resolveNativePDFOutput{}, resolutionRequestError(ctx, err)
		}
		return resolveNativePDFOutput{}, invalidToolArgumentsError()
	}
	c, err := lease.transferConnection(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return resolveNativePDFOutput{}, resolutionRequestError(ctx, err)
		}
		return resolveNativePDFOutput{}, daemonAcquisitionError(err)
	}
	defer func() { _ = lease.close(c) }()
	vault, err := c.API().VaultIdentity(ctx)
	if err != nil {
		return resolveNativePDFOutput{}, resolutionRequestError(ctx, err)
	}
	if !strings.EqualFold(input.VaultID.String(), vault.VaultID) {
		return resolveNativePDFOutput{}, &resolutionError{code: "redaction_selection_changed"}
	}
	review, err := c.ResolveNativePDF(ctx, request)
	output := resolveNativePDFOutput{privateCache: newPrivateCache(), Outcome: "review", ReviewSHA256: review.SHA256}
	var value any = review
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); ctx.Err() != nil || code != expansionRequired {
			return resolveNativePDFOutput{}, resolutionRequestError(ctx, err)
		}
		proposal, valid := checkedExpansion(request, err)
		if !valid {
			return resolveNativePDFOutput{}, &resolutionError{code: resolutionIntegrity}
		}
		value = proposal
		output.Outcome = "expansion_required"
		output.ReviewSHA256 = ""
		output.NextStep = "Inspect the proposal, edit the masks explicitly, and resolve to a new file."
	}
	data, err := json.Marshal(value)
	if err != nil {
		return resolveNativePDFOutput{}, &resolutionError{code: resolutionIntegrity}
	}
	data = append(data, '\n') // Match the CLI's JSON output.
	if int64(len(data)) > processing.MaxRedactionResponseBytes {
		return resolveNativePDFOutput{}, &resolutionError{code: "redaction_limit"}
	}
	output.redactionFileOutput, err = saveRedactionFile(ctx, logger, "docbank-resolution-", input.DestinationPath, data)
	if err != nil {
		if ctx.Err() != nil {
			return resolveNativePDFOutput{}, resolutionRequestError(ctx, err)
		}
		return resolveNativePDFOutput{}, resolutionDestinationError(err)
	}
	return output, nil
}

func checkedExpansion(request processing.RedactionResolveRequest, err error) (*redaction.Problem, bool) {
	status, _ := daemonconn.ResponseStatus(err)
	proposal, ok := errors.AsType[*redaction.Problem](err)
	if status != http.StatusUnprocessableEntity || !ok || proposal.Code != expansionRequired {
		return nil, false
	}
	// The daemon problem decoder permits unknown members. Check every decoded
	// field against the rebuilt proposal and publish only the rebuilt value.
	_, rebuiltErr := processing.BuildRedactionReview(request.Preparation, request.Masks,
		request.Preparation.Selection.Revision, pdfproduction.QualifiedRecipe())
	rebuilt, ok := errors.AsType[*redaction.Problem](rebuiltErr)
	if !ok || !reflect.DeepEqual(proposal, rebuilt) {
		return nil, false
	}
	return rebuilt, true
}

func resolutionRequestError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &resolutionError{code: "redaction_timeout"}
	case ctx.Err() != nil:
		return &resolutionError{code: "redaction_canceled"}
	case errors.Is(err, pdfproduction.ErrVerifierVersion):
		return &resolutionError{code: "redaction_verifier_version"}
	case errors.Is(err, daemonconn.ErrIntegrity), daemonconn.IsResponseDecodeError(err):
		return &resolutionError{code: resolutionIntegrity}
	case errors.Is(err, processing.ErrRedactionInvalidRequest), errors.Is(err, processing.ErrRedactionInvalidPreparation):
		return invalidToolArgumentsError()
	}
	code, _ := daemonconn.ProblemCode(err)
	if code == "invalid_redaction_request" || code == "invalid_redaction_preparation" {
		return invalidToolArgumentsError()
	}
	if _, known := resolutionMessages[code]; known && code != resolutionLocalIO {
		return &resolutionError{code: code}
	}
	return sanitizedDaemonError(errDaemonRequestFailed, err)
}

func resolutionDestinationError(err error) error {
	if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok {
		return invalid
	}
	return &resolutionError{code: resolutionLocalIO, cause: err}
}

func resolveNativePDFSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	input := rootObjectSchema(schema{
		schemaVaultIDField: uuidSchema(), "preparation_path": path, "masks_path": path, destinationPathField: path,
	}, schemaVaultIDField, "preparation_path", "masks_path", destinationPathField)
	output := rootObjectSchema(withPrivateCache(schema{
		"outcome": enumSchema("review", "expansion_required"), "review_sha256": sha256Schema(), "next_step": stringSchema(256),
		destinationPathField: path, schemaSizeField: integerSchema(1, processing.MaxRedactionResponseBytes), "sha256": sha256Schema(),
		schemaStateField: enumSchema("published", "published_durability_unknown"), "cleanup_failed": booleanSchema(),
	}), cacheRequired("outcome", destinationPathField, schemaSizeField, "sha256", schemaStateField, "cleanup_failed")...)
	return input, output
}
