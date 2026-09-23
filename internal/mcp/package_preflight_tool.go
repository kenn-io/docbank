package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

type packagePreflightSource struct {
	kind string
	path string
}

type preflightLoadFilePackageInput struct {
	SourcePath     string `json:"source_path"`
	Profile        string `json:"profile"`
	PageMapProfile string `json:"page_map_profile"`
	Encoding       string `json:"encoding"`
	MappingJSON    string `json:"mapping_json"`
}

type packagePreflightOutput struct {
	privateCache
	api.PackagePreflight
}

type packageDiagnosticPageOutput struct {
	privateCache
	api.PackageDiagnosticPage
}

func classifyPackagePreflightSource(source string) (packagePreflightSource, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return packagePreflightSource{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return packagePreflightSource{}, err
	}
	if info.IsDir() {
		return packagePreflightSource{kind: "root", path: absolute}, nil
	}
	if info.Mode().IsRegular() && filepath.Ext(absolute) == ".zip" {
		return packagePreflightSource{kind: "zip", path: absolute}, nil
	}
	return packagePreflightSource{}, errors.New("load-file source must be a directory or .zip file")
}

func preflightLoadFilePackage(
	ctx context.Context, lease *daemonLease, raw []byte,
) (packagePreflightOutput, error) {
	var input preflightLoadFilePackageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return packagePreflightOutput{}, err
	}
	source, err := classifyPackagePreflightSource(input.SourcePath)
	if err != nil {
		return packagePreflightOutput{}, invalidToolArgumentsError()
	}
	request := api.PackagePreflightRequest{Profile: input.Profile, PageMapProfile: input.PageMapProfile,
		Encoding: input.Encoding, SourceKind: "root", SourceRef: source.path}
	if input.MappingJSON != "" {
		var mapping loadfile.Mapping
		if len(input.MappingJSON) > loadfile.MaxMappingBytes ||
			json.Unmarshal([]byte(input.MappingJSON), &mapping, json.RejectUnknownMembers(true)) != nil ||
			mapping.Contract != loadfile.MappingContractV1 {
			return packagePreflightOutput{}, invalidToolArgumentsError()
		}
		request.Mapping = []byte(input.MappingJSON)
	}
	var result *api.PackagePreflight
	if source.kind == "root" {
		result, err = daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.PackagePreflight, error) {
			return c.API().CreatePackagePreflight(ctx, &apiclient.CreatePackagePreflightRequestOptions{Body: &request})
		})
	} else {
		result, err = preflightPackageZIP(ctx, lease, source.path, request)
	}
	if err != nil {
		return packagePreflightOutput{}, err
	}
	if err := validatePackagePreflightResult(*result); err != nil {
		return packagePreflightOutput{}, err
	}
	return packagePreflightOutput{PackagePreflight: *result, privateCache: newPrivateCache()}, nil
}

func preflightPackageZIP(
	ctx context.Context, lease *daemonLease, path string, request api.PackagePreflightRequest,
) (*api.PackagePreflight, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() < 1 || info.Size() > store.MailboxChunkBytes*store.MailboxMaxChunks {
		return nil, errors.New("ZIP source is outside the supported container size")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	containerID := uuid.New().String()
	hash := hex.EncodeToString(digest.Sum(nil))
	err = daemonProcessingStartVoid(ctx, lease, func(c *daemonconn.Connection) error {
		declared, err := c.API().BeginPackageContainer(ctx, &apiclient.BeginPackageContainerRequestOptions{
			Body: &apiclient.PackageContainerInput{ContainerID: containerID, Sha256: hash, Size: info.Size()},
		})
		if err != nil {
			return err
		}
		if declared.ContainerID != containerID || declared.SHA256 != hash || declared.Size != info.Size() {
			return errors.New("package container response does not bind its declaration")
		}
		buffer := make([]byte, store.MailboxChunkBytes)
		for index := 0; ; index++ {
			read, readErr := io.ReadFull(file, buffer)
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				return readErr
			}
			if read == 0 {
				break
			}
			chunk := append([]byte(nil), buffer[:read]...)
			chunkDigest := sha256.Sum256(chunk)
			_, err = c.API().UploadPackageChunk(ctx, &apiclient.UploadPackageChunkRequestOptions{
				PathParams: &apiclient.UploadPackageChunkPath{ID: containerID, Index: index},
				Header:     &apiclient.UploadPackageChunkHeaders{XDocbankBlobHash: hex.EncodeToString(chunkDigest[:]), XDocbankBlobSize: int64(read)},
			}, func(_ context.Context, request *http.Request) error {
				request.Body = io.NopCloser(bytes.NewReader(chunk))
				return nil
			})
			if err != nil {
				return err
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
		}
		sealed, err := c.API().SealPackageContainer(ctx, &apiclient.SealPackageContainerRequestOptions{
			PathParams: &apiclient.SealPackageContainerPath{ID: containerID},
		})
		if err != nil {
			return err
		}
		if sealed.State != "sealed" || sealed.SHA256 != hash || sealed.Size != info.Size() {
			return errors.New("sealed package container does not bind uploaded bytes")
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			container, readErr := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageContainer, error) {
				return c.API().GetPackageContainer(ctx, &apiclient.GetPackageContainerRequestOptions{
					PathParams: &apiclient.GetPackageContainerPath{ID: containerID},
				})
			})
			if readErr == nil && container.State == "sealed" && container.SHA256 == hash && container.Size == info.Size() {
				err = nil
			}
		}
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			// Abort decides atomically whether the owned source is unfinished;
			// sealed authority survives even when outcome readback failed.
			_ = daemonProcessingStartVoid(cleanup, lease, func(c *daemonconn.Connection) error {
				_, abortErr := c.API().AbortPackageContainer(cleanup, &apiclient.AbortPackageContainerRequestOptions{
					PathParams: &apiclient.AbortPackageContainerPath{ID: containerID},
				})
				return abortErr
			})
			return nil, err
		}
	}
	request.SourceKind, request.SourceRef = "container", containerID
	return daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.PackagePreflight, error) {
		return c.API().PreflightPackageContainer(ctx, &apiclient.PreflightPackageContainerRequestOptions{
			PathParams: &apiclient.PreflightPackageContainerPath{ID: containerID}, Body: &request,
		})
	})
}

func daemonProcessingStartVoid(ctx context.Context, lease *daemonLease, callback func(*daemonconn.Connection) error) error {
	_, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*struct{}, error) {
		if err := callback(c); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})
	return err
}

func validatePackagePreflightResult(value api.PackagePreflight) error {
	if _, err := uuid.Parse(value.PreflightID); err != nil || value.SourceKind == "" || value.SourceRef == "" ||
		!canonical.IsSHA256Hex(value.ProfileSHA256) || !canonical.IsSHA256Hex(value.MappingSHA256) ||
		!canonical.IsSHA256Hex(value.ManifestSHA256) || value.Records < 0 || value.Pages < 0 ||
		value.DiagnosticCount < len(value.Diagnostics) || len(value.Diagnostics) > maxPackageDiagnostics || value.CreatedAt == "" || value.ExpiresAt == "" {
		return errors.New("package preflight response does not bind retained authority")
	}
	return nil
}

func getPackagePreflight(ctx context.Context, lease *daemonLease, raw []byte) (packagePreflightOutput, error) {
	var input struct {
		PreflightID string `json:"preflight_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return packagePreflightOutput{}, err
	}
	id, err := uuid.Parse(input.PreflightID)
	if err != nil {
		return packagePreflightOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackagePreflight, error) {
		return c.API().ReadPackagePreflight(ctx, &apiclient.ReadPackagePreflightRequestOptions{
			PathParams: &apiclient.ReadPackagePreflightPath{PreflightID: id},
		})
	})
	if err != nil {
		return packagePreflightOutput{}, err
	}
	if result.PreflightID != input.PreflightID {
		return packagePreflightOutput{}, errors.New("package preflight response changed identity")
	}
	if err := validatePackagePreflightResult(*result); err != nil {
		return packagePreflightOutput{}, err
	}
	return packagePreflightOutput{PackagePreflight: *result, privateCache: newPrivateCache()}, nil
}

func listPackagePreflightDiagnostics(ctx context.Context, lease *daemonLease, raw []byte) (packageDiagnosticPageOutput, error) {
	var input struct {
		PreflightID string `json:"preflight_id"`
		Cursor      string `json:"cursor"`
		Limit       int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return packageDiagnosticPageOutput{}, err
	}
	id, err := uuid.Parse(input.PreflightID)
	if err != nil {
		return packageDiagnosticPageOutput{}, invalidToolArgumentsError()
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageDiagnosticPage, error) {
		return c.API().ReadPackagePreflightDiagnostics(ctx, &apiclient.ReadPackagePreflightDiagnosticsRequestOptions{
			PathParams: &apiclient.ReadPackagePreflightDiagnosticsPath{PreflightID: id},
			Query:      &apiclient.ReadPackagePreflightDiagnosticsQuery{Limit: &input.Limit, Cursor: optionalString(input.Cursor)},
		})
	})
	if err != nil {
		return packageDiagnosticPageOutput{}, err
	}
	if len(page.Diagnostics) > input.Limit || page.Total < len(page.Diagnostics) {
		return packageDiagnosticPageOutput{}, errors.New("package diagnostic page exceeded its requested bound")
	}
	if page.Diagnostics == nil {
		page.Diagnostics = []api.PackageDiagnostic{}
	}
	return packageDiagnosticPageOutput{PackageDiagnosticPage: *page, privateCache: newPrivateCache()}, nil
}

func packagePreflightToolHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := preflightLoadFilePackage(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, preflightLoadFilePackageToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
