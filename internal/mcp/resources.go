package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const renditionResourceTemplate = "docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}{?offset,max_chars}"

const resourceCatalogTTLMs = 60_000

type renditionResourceIdentity struct {
	VaultID          string
	NodeID           int64
	ContentVersionID string
	AttachmentID     string
}

type renditionWindow struct {
	Offset   int
	MaxChars int
}

func renditionResourceURI(identity renditionResourceIdentity) string {
	return "docbank://vaults/" + identity.VaultID + "/documents/" +
		strconv.FormatInt(identity.NodeID, 10) + "/versions/" + identity.ContentVersionID +
		"/renditions/" + identity.AttachmentID
}

func parseRenditionResourceURI(raw string) (renditionResourceIdentity, renditionWindow, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "docbank" || parsed.Host != "vaults" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.EscapedPath() != parsed.Path {
		return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource URI")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 7 || parts[0] == "" || parts[1] != "documents" ||
		parts[3] != "versions" || parts[5] != "renditions" {
		return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource URI")
	}
	nodeID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || nodeID < 1 || strconv.FormatInt(nodeID, 10) != parts[2] ||
		!validUUIDv4Identity(parts[0]) || !validUUIDv4Identity(parts[4]) || !validSHA256Identity(parts[6]) {
		return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource identity")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource window")
	}
	for key, values := range query {
		if (key != "offset" && key != "max_chars") || len(values) != 1 || values[0] == "" {
			return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource window")
		}
	}
	window := renditionWindow{MaxChars: defaultRenditionChars}
	if value := query.Get("offset"); value != "" {
		window.Offset, err = strconv.Atoi(value)
		if err != nil || window.Offset < 0 || strconv.Itoa(window.Offset) != value || window.Offset > 1<<31-1 {
			return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource offset")
		}
	}
	if value := query.Get("max_chars"); value != "" {
		window.MaxChars, err = strconv.Atoi(value)
		if err != nil || window.MaxChars < 1 || window.MaxChars > maxRenditionChars || strconv.Itoa(window.MaxChars) != value {
			return renditionResourceIdentity{}, renditionWindow{}, errors.New("invalid rendition resource maximum")
		}
	}
	return renditionResourceIdentity{VaultID: parts[0], NodeID: nodeID,
		ContentVersionID: parts[4], AttachmentID: parts[6]}, window, nil
}

func validUUIDv4Identity(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validSHA256Identity(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func registerResourceSurface(server *sdkmcp.Server, lease *daemonLease) {
	server.AddResourceTemplate(&sdkmcp.ResourceTemplate{
		URITemplate: renditionResourceTemplate,
		Name:        "docbank-rendition-text",
		Title:       "Docbank rendition text",
		Description: "Read one bounded Unicode window from an exact active sanitized Markdown rendition.",
		MIMEType:    "text/markdown",
		Meta: sdkmcp.Meta{"io.docbank/bounds": map[string]any{
			"defaultMaxChars": defaultRenditionChars, "maxChars": maxRenditionChars,
			"maxResponseBytes": maxToolResponseBytes,
		}},
	}, renditionResourceHandler(lease))
}

func renditionResourceHandler(lease *daemonLease) sdkmcp.ResourceHandler {
	return func(ctx context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		if request == nil || request.Params == nil {
			return nil, sanitizedRPCError(errors.New("missing resource request"))
		}
		identity, requested, err := parseRenditionResourceURI(request.Params.URI)
		if err != nil {
			return nil, sdkmcp.ResourceNotFoundError(request.Params.URI)
		}
		window, err := daemonRead(ctx, lease, func(c *client.Client) (api.RenditionTextWindow, error) {
			return c.RenditionTextWindow(ctx, api.RenditionWindowRequest{
				VaultID: identity.VaultID, NodeID: identity.NodeID,
				ContentVersionID: identity.ContentVersionID, AttachmentID: identity.AttachmentID,
				Offset: requested.Offset, MaxChars: requested.MaxChars,
			})
		})
		if err != nil {
			if code, _ := stableDomainError(err); code != "" {
				return nil, sdkmcp.ResourceNotFoundError(request.Params.URI)
			}
			return nil, sanitizedRPCError(err)
		}
		result := &sdkmcp.ReadResourceResult{
			TTLMs: 0, CacheScope: "private",
			Contents: []*sdkmcp.ResourceContents{{
				URI: request.Params.URI, MIMEType: "text/markdown", Text: window.Text,
				Meta: sdkmcp.Meta{
					"vaultId": window.VaultID, "nodeId": window.NodeID,
					"contentVersionId": window.ContentVersionID, "attachmentId": window.AttachmentID,
					"buildId": window.BuildID, "profileFingerprint": window.ProfileFingerprint,
					"checksum": window.Checksum, "requestedOffset": window.RequestedOffset,
					"actualStart": window.ActualStart, "actualEnd": window.ActualEnd,
					"nextOffset": window.NextOffset, "eof": window.EOF,
					"responseBytes": window.ResponseBytes,
				},
			}},
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil || len(encoded) > maxToolResponseBytes {
			return nil, sanitizedRPCError(errToolResultTooLarge)
		}
		return result, nil
	}
}

func normalizeResourceCatalogs(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil {
			return result, err
		}
		switch catalog := result.(type) {
		case *sdkmcp.ListResourcesResult:
			if method == "resources/list" {
				catalog.TTLMs, catalog.CacheScope = resourceCatalogTTLMs, "public"
			}
		case *sdkmcp.ListResourceTemplatesResult:
			if method == "resources/templates/list" {
				catalog.TTLMs, catalog.CacheScope = resourceCatalogTTLMs, "public"
			}
		case *sdkmcp.ReadResourceResult:
			if method == "resources/read" {
				catalog.TTLMs, catalog.CacheScope = 0, "private"
			}
		}
		return result, nil
	}
}
