package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/daemonconn"
)

const (
	maxReportChunkBytes  = 256 << 10
	reportHandleLifetime = 15 * time.Minute
	maxReportHandleBytes = 512
)

var errReportHandleUnavailable = errors.New("report artifact handle is unavailable")

type reportHandleSigner struct {
	key           [32]byte
	mu            sync.Mutex
	spools        map[string]*reportSpool
	reservedBytes int64
	opening       int
	closed        bool
}

type reportHandle struct {
	ID      string `json:"id"`
	Format  string `json:"format"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Expires int64  `json:"expires"`
	Nonce   string `json:"nonce"`
}

func newReportHandleSigner() *reportHandleSigner {
	signer := &reportHandleSigner{spools: make(map[string]*reportSpool)}
	if _, err := rand.Read(signer.key[:]); err != nil {
		panic("generating report handle signing key: " + err.Error())
	}
	return signer
}

func (signer *reportHandleSigner) sign(handle *reportHandle) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating report handle nonce: %w", err)
	}
	handle.Nonce = base64.RawURLEncoding.EncodeToString(nonce[:])
	data, err := json.Marshal(handle)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, signer.key[:])
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (signer *reportHandleSigner) verify(token string) (reportHandle, error) {
	if len(token) > maxReportHandleBytes {
		return reportHandle{}, errReportHandleUnavailable
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok {
		return reportHandle{}, errReportHandleUnavailable
	}
	data, dataErr := base64.RawURLEncoding.DecodeString(encoded)
	got, signatureErr := base64.RawURLEncoding.DecodeString(signature)
	if dataErr != nil || signatureErr != nil || len(data) > maxReportHandleBytes {
		return reportHandle{}, errReportHandleUnavailable
	}
	mac := hmac.New(sha256.New, signer.key[:])
	_, _ = mac.Write(data)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return reportHandle{}, errReportHandleUnavailable
	}
	var handle reportHandle
	if err := json.Unmarshal(data, &handle, json.RejectUnknownMembers(true)); err != nil {
		return reportHandle{}, errReportHandleUnavailable
	}
	decodedNonce, nonceErr := base64.RawURLEncoding.DecodeString(handle.Nonce)
	if len(handle.ID) != 48 || len(handle.SHA256) != 64 || handle.Size < 1 || handle.Size > 512<<20 ||
		(handle.Format != "csv" && handle.Format != "bundle") || time.Now().Unix() >= handle.Expires ||
		nonceErr != nil || len(decodedNonce) != 16 {
		return reportHandle{}, errReportHandleUnavailable
	}
	return handle, nil
}

func openReportArtifactSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"report_id": reportIDSchema(),
		"format":    enumSchema("csv", "bundle"),
	}, "report_id", "format")
	output := rootObjectSchema(withPrivateCache(schema{
		"handle": stringSchema(maxReportHandleBytes), "report_id": stringSchema(48),
		"format": enumSchema("csv", "bundle"), "size": integerSchema(1, 512<<20),
		"sha256": sha256Schema(), "expires_at": dateTimeSchema(),
	}), cacheRequired("handle", "report_id", "format", "size", "sha256", "expires_at")...)
	return input, output
}

func downloadReportArtifactSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"handle":    stringSchema(maxReportHandleBytes),
		"offset":    integerSchema(0, 512<<20),
		"max_bytes": integerSchema(1, maxReportChunkBytes),
		"close":     reportBoolSchema(),
	}, "handle", "offset", "max_bytes")
	output := rootObjectSchema(withPrivateCache(schema{
		"data_base64": stringSchema(base64.StdEncoding.EncodedLen(maxReportChunkBytes)),
		"offset":      integerSchema(0, 512<<20), "next_offset": integerSchema(0, 512<<20),
		"total_bytes": integerSchema(1, 512<<20), "sha256": sha256Schema(),
		"eof": reportBoolSchema(), "closed": reportBoolSchema(),
	}), cacheRequired("data_base64", "offset", "next_offset", "total_bytes", "sha256", "eof", "closed")...)
	return input, output
}

func reportIDSchema() schema {
	result := stringSchema(48)
	result["pattern"] = "^[0-9a-f]{48}$"
	result["minLength"] = 48
	return result
}

func reportBoolSchema() schema {
	return schema{"type": "boolean"} //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
}

type openReportArtifactOutput struct {
	privateCache

	Handle    string `json:"handle"`
	ReportID  string `json:"report_id"`
	Format    string `json:"format"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt string `json:"expires_at"`
}

type downloadReportArtifactOutput struct {
	privateCache

	DataBase64 string `json:"data_base64"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	TotalBytes int64  `json:"total_bytes"`
	SHA256     string `json:"sha256"`
	EOF        bool   `json:"eof"`
	Closed     bool   `json:"closed"`
}

func reportToolHandler(lease *daemonLease, signer *reportHandleSigner, name string,
	validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case "open_report_artifact":
			output, err = openReportArtifact(ctx, lease, signer, request.Params.Arguments)
		case "download_report_artifact":
			output, err = downloadReportArtifact(ctx, lease, signer, request.Params.Arguments)
		default:
			err = errors.New("unknown report tool")
		}
		if err == nil {
			var result *sdkmcp.CallToolResult
			result, err = boundedToolSuccess(validator, output, nil)
			if err == nil {
				return result, nil
			}
		}
		logOperationError(logger, name, err)
		if domain, ok := domainToolError(err); ok {
			return domain, nil
		}
		return nil, sanitizedRPCError(err)
	}
}

type reportSpoolAttempt struct {
	spool    reportSpool
	localErr error
}

func openReportArtifact(ctx context.Context, lease *daemonLease, signer *reportHandleSigner,
	raw []byte,
) (openReportArtifactOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
		Format   string `json:"format"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return openReportArtifactOutput{}, err
	}
	attempt, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (reportSpoolAttempt, error) {
		stream, err := client.OpenTermReport(ctx, input.ReportID, input.Format)
		if err != nil {
			return reportSpoolAttempt{}, err
		}
		defer func() { _ = stream.Close() }()
		if err := signer.reserve(stream.Size); err != nil {
			return reportSpoolAttempt{localErr: err}, nil
		}
		spool, err := createVerifiedReportSpool(ctx, stream, input.Format)
		if err != nil {
			signer.abort(stream.Size)
			return reportSpoolAttempt{}, err
		}
		spool.size = stream.Size
		spool.metadata = reportHandle{ID: input.ReportID, Format: input.Format,
			Size: stream.Size, SHA256: stream.SHA256}
		return reportSpoolAttempt{spool: spool}, nil
	})
	if err != nil {
		return openReportArtifactOutput{}, err
	}
	if attempt.localErr != nil {
		return openReportArtifactOutput{}, attempt.localErr
	}
	spool := attempt.spool
	expires := time.Now().Add(reportHandleLifetime).UTC().Truncate(time.Second)
	spool.metadata.Expires = expires.Unix()
	token, err := signer.sign(&spool.metadata)
	if err != nil {
		signer.discardUnpublished(&spool)
		return openReportArtifactOutput{}, err
	}
	if err := signer.publish(token, &spool); err != nil {
		return openReportArtifactOutput{}, err
	}
	return openReportArtifactOutput{privateCache: newPrivateCache(), Handle: token,
		ReportID: spool.metadata.ID, Format: spool.metadata.Format, Size: spool.metadata.Size,
		SHA256: spool.metadata.SHA256, ExpiresAt: expires.Format(time.RFC3339)}, nil
}

func downloadReportArtifact(ctx context.Context, lease *daemonLease, signer *reportHandleSigner,
	raw []byte,
) (downloadReportArtifactOutput, error) {
	var input struct {
		Handle   string `json:"handle"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"max_bytes"`
		Close    bool   `json:"close"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return downloadReportArtifactOutput{}, err
	}
	handle, err := signer.verify(input.Handle)
	if err != nil || input.Offset < 0 || input.Offset > handle.Size ||
		input.MaxBytes < 1 || input.MaxBytes > maxReportChunkBytes {
		return downloadReportArtifactOutput{}, errReportHandleUnavailable
	}
	if input.Close {
		// An explicit close still releases the private spool if the daemon
		// recheck fails before a chunk can be read.
		defer signer.drop(input.Handle)
	}
	metadata, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (reportHandle, error) {
		// The summary uses the same owner-bound cache authority as artifact
		// acquisition, without initiating another artifact stream.
		summary, err := client.GetTermReport(ctx, handle.ID)
		if err != nil {
			return reportHandle{}, err
		}
		if handle.Format == "csv" {
			return reportHandle{Size: summary.CSVBytes, SHA256: summary.CSVSHA256}, nil
		}
		return reportHandle{Size: summary.BundleBytes, SHA256: summary.BundleSHA256}, nil
	})
	if err != nil {
		code, _ := stableDomainError(err)
		switch code {
		case "report_unavailable", "visibility_changed", "access_denied", "not_found":
			signer.drop(input.Handle)
		}
		return downloadReportArtifactOutput{}, err
	}
	if metadata.Size != handle.Size || metadata.SHA256 != handle.SHA256 {
		signer.drop(input.Handle)
		return downloadReportArtifactOutput{}, errReportHandleUnavailable
	}
	chunk, err := signer.read(input.Handle, handle, input.Offset, input.MaxBytes, input.Close)
	if err != nil {
		return downloadReportArtifactOutput{}, err
	}
	next := input.Offset + int64(len(chunk))
	return downloadReportArtifactOutput{privateCache: newPrivateCache(),
		DataBase64: base64.StdEncoding.EncodeToString(chunk), Offset: input.Offset,
		NextOffset: next, TotalBytes: handle.Size, SHA256: handle.SHA256,
		EOF: next == handle.Size, Closed: input.Close}, nil
}
