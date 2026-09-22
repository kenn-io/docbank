package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfstamp"
)

var errBatesOutcomeUnknown = errors.New("the Bates authority outcome is unknown; reconcile before retrying")

const (
	batesExportMediaType      = "application/pdf"
	maxBatesCandidateEvidence = 25
	maxBatesFileBytes         = 512 << 20
)

type listBatesNamespacesInput struct {
	Cursor string `json:"cursor"`
	Limit  int64  `json:"limit"`
}

type batesNamespacePageOutput struct {
	privateCache

	api.BatesNamespacePage
}

type batesNamespaceOutput struct {
	privateCache

	api.BatesNamespace
}

type batesPlanInput struct {
	OperationID  string `json:"operation_id"`
	NamespaceID  string `json:"namespace_id"`
	SnapshotID   string `json:"snapshot_id"`
	RecipeSHA256 string `json:"recipe_sha256"`
	StartAt      int64  `json:"start_at"`
}

func (in batesPlanInput) request() api.BatesPlanRequest {
	return api.BatesPlanRequest{OperationID: in.OperationID, NamespaceID: in.NamespaceID,
		SnapshotID: in.SnapshotID, RecipeSHA256: in.RecipeSHA256, StartAt: in.StartAt}
}

type batesPlanOutput struct {
	privateCache

	api.BatesPlan
}

type batesAllocationOutput struct {
	privateCache

	api.BatesAllocation
}

type listBatesExportsInput struct {
	After string `json:"after"`
	Limit int64  `json:"limit"`
}

type batesExportOutput struct {
	privateCache

	api.BatesExport
}

type batesExportPageOutput struct {
	privateCache

	api.BatesExportPage
}

type findBatesExportsInput struct {
	BatesLabel     string `json:"bates_label"`
	CustodianLabel string `json:"custodian_label"`
	PersonID       string `json:"person_id"`
	Cursor         string `json:"cursor"`
	Limit          int64  `json:"limit"`
}

type batesCandidatePageOutput struct {
	privateCache

	api.BatesCandidatePage
}

type exportBatesFileInput struct {
	AllocationID    string `json:"allocation_id"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
}

type exportBatesFileOutput struct {
	privateCache

	AllocationID    string `json:"allocation_id"`
	ArtifactID      string `json:"artifact_id"`
	DestinationPath string `json:"destination_path"`
	BlobSHA256      string `json:"blob_sha256"`
	ManifestSHA256  string `json:"manifest_sha256"`
	Size            int64  `json:"size"`
	State           string `json:"state"`
}

type publishBatesExportInput struct {
	AllocationID string          `json:"allocation_id"`
	Recipe       pdfstamp.Recipe `json:"recipe"`
}

func listBatesNamespaces(ctx context.Context, lease *daemonLease, raw []byte) (batesNamespacePageOutput, error) {
	var input listBatesNamespacesInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesNamespacePageOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesNamespacePage, error) {
		return c.API().ListBatesNamespaces(ctx, &apiclient.ListBatesNamespacesRequestOptions{
			Query: &apiclient.ListBatesNamespacesQuery{Cursor: &input.Cursor, Limit: &input.Limit},
		})
	})
	if err != nil {
		return batesNamespacePageOutput{}, err
	}
	if int64(len(page.Items)) > input.Limit {
		return batesNamespacePageOutput{}, errors.New("bates namespace page exceeded its requested bound")
	}
	if page.Items == nil {
		page.Items = []api.BatesNamespace{}
	}
	return batesNamespacePageOutput{BatesNamespacePage: *page, privateCache: newPrivateCache()}, nil
}

func previewBatesStamp(ctx context.Context, lease *daemonLease, raw []byte) (batesPlanOutput, error) {
	var input batesPlanInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesPlanOutput{}, err
	}
	plan, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesPlan, error) {
		request := input.request()
		return c.API().PlanBatesStamp(ctx, &apiclient.PlanBatesStampRequestOptions{Body: &request})
	})
	if err != nil {
		return batesPlanOutput{}, err
	}
	if plan.Namespace.NamespaceID != input.NamespaceID || !plan.StampedNothing || plan.AllocationID != "" ||
		len(plan.Labels) == 0 || len(plan.Labels) > maxBatesLabels {
		return batesPlanOutput{}, errors.New("bates preview response does not bind its reviewed request")
	}
	return batesPlanOutput{BatesPlan: *plan, privateCache: newPrivateCache()}, nil
}

func getBatesAllocation(ctx context.Context, lease *daemonLease, raw []byte) (batesAllocationOutput, error) {
	var input struct {
		AllocationID string `json:"allocation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesAllocationOutput{}, err
	}
	id, err := uuid.Parse(input.AllocationID)
	if err != nil {
		return batesAllocationOutput{}, invalidToolArgumentsError()
	}
	allocation, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesAllocation, error) {
		return c.API().ReadBatesAllocation(ctx, &apiclient.ReadBatesAllocationRequestOptions{
			PathParams: &apiclient.ReadBatesAllocationPath{ID: id},
		})
	})
	if err != nil {
		return batesAllocationOutput{}, err
	}
	if allocation.AllocationID != input.AllocationID {
		return batesAllocationOutput{}, errors.New("bates allocation response does not bind its requested identity")
	}
	return batesAllocationOutput{BatesAllocation: *allocation, privateCache: newPrivateCache()}, nil
}

func listBatesExports(ctx context.Context, lease *daemonLease, raw []byte) (batesExportPageOutput, error) {
	var input listBatesExportsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesExportPageOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesExportPage, error) {
		return c.API().ListBatesExports(ctx, &apiclient.ListBatesExportsRequestOptions{
			Query: &apiclient.ListBatesExportsQuery{After: &input.After, Limit: &input.Limit},
		})
	})
	if err != nil {
		return batesExportPageOutput{}, err
	}
	if int64(len(page.Items)) > input.Limit || page.Total < len(page.Items) {
		return batesExportPageOutput{}, errors.New("bates export history exceeded its requested bound")
	}
	for _, item := range page.Items {
		if err := validateBatesExport(item, ""); err != nil {
			return batesExportPageOutput{}, err
		}
	}
	if page.NextAfter != "" {
		if _, err := uuid.Parse(page.NextAfter); err != nil {
			return batesExportPageOutput{}, errors.New("bates export history returned an invalid cursor")
		}
	}
	if page.Items == nil {
		page.Items = []api.BatesExport{}
	}
	return batesExportPageOutput{BatesExportPage: *page, privateCache: newPrivateCache()}, nil
}

func getBatesExport(ctx context.Context, lease *daemonLease, raw []byte) (batesExportOutput, error) {
	var input struct {
		AllocationID string `json:"allocation_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesExportOutput{}, err
	}
	id, err := uuid.Parse(input.AllocationID)
	if err != nil {
		return batesExportOutput{}, invalidToolArgumentsError()
	}
	artifact, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesExport, error) {
		return c.API().ReadBatesExport(ctx, &apiclient.ReadBatesExportRequestOptions{
			PathParams: &apiclient.ReadBatesExportPath{ID: id},
		})
	})
	if err != nil {
		return batesExportOutput{}, err
	}
	if err := validateBatesExport(*artifact, input.AllocationID); err != nil {
		return batesExportOutput{}, err
	}
	return batesExportOutput{BatesExport: *artifact, privateCache: newPrivateCache()}, nil
}

func findBatesExports(ctx context.Context, lease *daemonLease, raw []byte) (batesCandidatePageOutput, error) {
	var input findBatesExportsInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesCandidatePageOutput{}, err
	}
	selected := 0
	for _, value := range []string{input.BatesLabel, input.CustodianLabel, input.PersonID} {
		if value != "" {
			selected++
		}
	}
	if selected != 1 {
		return batesCandidatePageOutput{}, invalidToolArgumentsError()
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesCandidatePage, error) {
		return c.API().FindBatesExports(ctx, &apiclient.FindBatesExportsRequestOptions{Query: &apiclient.FindBatesExportsQuery{
			BatesLabel: optionalString(input.BatesLabel), CustodianLabel: optionalString(input.CustodianLabel),
			PersonID: optionalString(input.PersonID), Cursor: optionalString(input.Cursor), Limit: &input.Limit,
		}})
	})
	if err != nil {
		return batesCandidatePageOutput{}, err
	}
	if int64(len(page.Items)) > input.Limit {
		return batesCandidatePageOutput{}, errors.New("bates candidate page exceeded its requested bound")
	}
	for _, candidate := range page.Items {
		if candidate.ArtifactID == "" || candidate.AllocationID == "" || candidate.SnapshotID == "" ||
			candidate.State != "verified" || candidate.MediaType != batesExportMediaType || candidate.Size < 1 ||
			candidate.PageCount < 1 || !validBatesDigest(candidate.BlobSHA256) || !validBatesDigest(candidate.ManifestSHA256) ||
			len(candidate.Evidence) == 0 || len(candidate.Evidence) > maxBatesCandidateEvidence {
			return batesCandidatePageOutput{}, errors.New("bates candidate response does not bind verified evidence")
		}
	}
	if page.Items == nil {
		page.Items = []api.BatesCandidate{}
	}
	return batesCandidatePageOutput{BatesCandidatePage: *page, privateCache: newPrivateCache()}, nil
}

func exportBatesFile(ctx context.Context, lease *daemonLease, raw []byte) (exportBatesFileOutput, error) {
	var input exportBatesFileInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportBatesFileOutput{}, err
	}
	if _, err := uuid.Parse(input.AllocationID); err != nil || !filepath.IsAbs(input.DestinationPath) ||
		filepath.Clean(input.DestinationPath) != input.DestinationPath {
		return exportBatesFileOutput{}, invalidToolArgumentsError()
	}
	parent := filepath.Dir(input.DestinationPath)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return exportBatesFileOutput{}, invalidToolArgumentsError()
	}
	if destination, err := os.Lstat(input.DestinationPath); err == nil {
		if destination.Mode()&os.ModeSymlink != 0 || !destination.Mode().IsRegular() {
			return exportBatesFileOutput{}, invalidToolArgumentsError()
		}
		if !input.Overwrite {
			return exportBatesFileOutput{}, os.ErrExist
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return exportBatesFileOutput{}, err
	}
	stage, err := filepublish.CreateStage(parent, ".docbank-bates-")
	if err != nil {
		return exportBatesFileOutput{}, err
	}
	staged, stagedPath := stage.File, stage.Path()
	keep := false
	defer func() {
		if !keep {
			_ = stage.Cleanup()
		}
	}()
	var verifiedSize int64
	receipt, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.BatesExport, error) {
		receipt, err := c.BatesExport(ctx, input.AllocationID)
		if err != nil {
			return nil, err
		}
		if receipt.Size > maxBatesFileBytes {
			return nil, errors.New("Bates export exceeds the local file export limit") //nolint:staticcheck // Bates is a proper name.
		}
		verified, err := c.DownloadBatesExportTo(ctx, receipt, staged)
		verifiedSize = verified.Size
		return &receipt, err
	})
	if err != nil {
		return exportBatesFileOutput{}, err
	}
	if verifiedSize != receipt.Size {
		return exportBatesFileOutput{}, errors.New("verified Bates export size changed")
	}
	if err := staged.Sync(); err != nil {
		return exportBatesFileOutput{}, err
	}
	if err := staged.Close(); err != nil {
		return exportBatesFileOutput{}, err
	}
	published, publishErr := filepublish.Publish(stagedPath, input.DestinationPath, input.Overwrite)
	if !published {
		return exportBatesFileOutput{}, publishErr
	}
	_ = os.Remove(stagedPath)
	keep = true
	_ = stage.Cleanup()
	state := "published"
	if publishErr != nil {
		state = "published_durability_unknown"
	}
	return exportBatesFileOutput{privateCache: newPrivateCache(), AllocationID: receipt.AllocationID,
		ArtifactID: receipt.ArtifactID, DestinationPath: input.DestinationPath, BlobSHA256: receipt.BlobSHA256,
		ManifestSHA256: receipt.ManifestSHA256, Size: receipt.Size, State: state}, nil
}

func batesWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case ensureBatesNamespaceToolDefinition.name:
			output, err = ensureBatesNamespace(ctx, lease, request.Params.Arguments)
		case reserveBatesRangeToolDefinition.name:
			output, err = reserveBatesRange(ctx, lease, request.Params.Arguments)
		case publishBatesExportToolDefinition.name:
			output, err = publishBatesExport(ctx, lease, request.Params.Arguments)
		case exportBatesFileToolDefinition.name:
			output, err = exportBatesFile(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown Bates write tool")
		}
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}

func publishBatesExport(ctx context.Context, lease *daemonLease, raw []byte) (batesExportOutput, error) {
	var input publishBatesExportInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesExportOutput{}, err
	}
	artifact, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.BatesExport, error) {
		return c.API().PublishBatesExport(ctx, &apiclient.PublishBatesExportRequestOptions{
			Body: &api.BatesExportRequest{AllocationID: input.AllocationID, Recipe: input.Recipe},
		})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return batesExportOutput{}, errBatesOutcomeUnknown
		}
		return batesExportOutput{}, err
	}
	if err := validateBatesExport(*artifact, input.AllocationID); err != nil {
		return batesExportOutput{}, err
	}
	return batesExportOutput{BatesExport: *artifact, privateCache: newPrivateCache()}, nil
}

func validateBatesExport(value api.BatesExport, requested string) error {
	if value.ArtifactID == "" || value.ArtifactID != value.AllocationID ||
		(requested != "" && value.AllocationID != requested) || value.State != "verified" ||
		value.MediaType != batesExportMediaType || value.Size < 1 || value.PageCount < 1 ||
		value.PageCount != len(value.Pages) || len(value.Pages) > maxBatesLabels ||
		!validBatesDigest(value.BlobSHA256) || !validBatesDigest(value.RecipeSHA256) ||
		!validBatesDigest(value.ManifestSHA256) {
		return errors.New("bates export response does not bind its verified authority")
	}
	if _, err := uuid.Parse(value.ArtifactID); err != nil {
		return errors.New("bates export response contains an invalid identity")
	}
	for index, page := range value.Pages {
		if page.Ordinal != index+1 || page.OutputPage != index+1 || page.SourcePage < 1 ||
			page.OccurrenceID == "" || page.Label == "" || !validBatesDigest(page.SourceBlobSHA256) {
			return errors.New("bates export response contains an invalid page receipt")
		}
	}
	return nil
}

func validBatesDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func ensureBatesNamespace(ctx context.Context, lease *daemonLease, raw []byte) (batesNamespaceOutput, error) {
	var input struct {
		Prefix  string `json:"prefix"`
		Suffix  string `json:"suffix"`
		Padding int    `json:"padding"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesNamespaceOutput{}, err
	}
	namespace, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.BatesNamespace, error) {
		return c.API().CreateBatesNamespace(ctx, &apiclient.CreateBatesNamespaceRequestOptions{
			Body: &api.BatesNamespaceRequest{Prefix: input.Prefix, Suffix: input.Suffix, Padding: input.Padding},
		})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return batesNamespaceOutput{}, errBatesOutcomeUnknown
		}
		return batesNamespaceOutput{}, err
	}
	if namespace.Prefix != input.Prefix || namespace.Suffix != input.Suffix || namespace.Padding != input.Padding {
		return batesNamespaceOutput{}, errors.New("bates namespace response does not bind its requested identity")
	}
	return batesNamespaceOutput{BatesNamespace: *namespace, privateCache: newPrivateCache()}, nil
}

func reserveBatesRange(ctx context.Context, lease *daemonLease, raw []byte) (batesAllocationOutput, error) {
	var input batesPlanInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return batesAllocationOutput{}, err
	}
	allocation, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.BatesAllocation, error) {
		request := input.request()
		return c.API().ReserveBatesRange(ctx, &apiclient.ReserveBatesRangeRequestOptions{Body: &request})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return batesAllocationOutput{}, errBatesOutcomeUnknown
		}
		return batesAllocationOutput{}, err
	}
	if allocation.NamespaceID != input.NamespaceID || allocation.SnapshotID != input.SnapshotID ||
		allocation.RecipeSHA256 != input.RecipeSHA256 || len(allocation.Labels) == 0 || len(allocation.Labels) > maxBatesLabels {
		return batesAllocationOutput{}, errors.New("bates reservation response does not bind its reviewed request")
	}
	return batesAllocationOutput{BatesAllocation: *allocation, privateCache: newPrivateCache()}, nil
}
