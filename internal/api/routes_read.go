package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

type nodeOutput struct {
	ETag string `header:"ETag"`
	Body Node
}

func contentDigest(hash []byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(hash) + ":"
}

func isContentCorruption(err error) bool {
	return errors.Is(err, packstore.ErrContentMismatch) ||
		errors.Is(err, pack.ErrTruncated) ||
		errors.Is(err, pack.ErrChecksum) ||
		errors.Is(err, pack.ErrCorrupt) ||
		errors.Is(err, pack.ErrBlobMismatch)
}

func staleRevisionError(id, expected, actual int64) error {
	return FromStoreError(fmt.Errorf("node %d revision is %d, expected %d: %w",
		id, actual, expected, store.ErrStaleRevision))
}

// nodeOutputAt builds the single-node response with a caller-supplied
// display path (used where the store-computed path would mislead, e.g.
// trash responses reporting the pre-trash location).
func nodeOutputAt(n store.Node, path string) *nodeOutput {
	body := fromStoreNode(n)
	body.Path = path
	return &nodeOutput{ETag: fmt.Sprintf("%q", strconv.FormatInt(n.Revision, 10)), Body: body}
}

// nodeWithPath loads the node's display path and builds the single-node
// response. Every single-node endpoint returns this shape.
func nodeWithPath(ctx context.Context, d Deps, id int64) (*nodeOutput, error) {
	view, err := d.Store.NodeSourceMetadataViewByID(ctx, id)
	if err != nil {
		return nil, FromStoreError(err)
	}
	return nodeOutputFromSourceMetadataView(ctx, view), nil
}

func nodeOutputFromSourceMetadataView(ctx context.Context, view store.NodeSourceMetadataView) *nodeOutput {
	out := nodeOutputAt(view.Node, view.Path)
	if view.SourceMetadata != nil {
		out.Body.SourceMetadata = fromStoreSourceMetadata(*view.SourceMetadata, browserSessionRequest(ctx))
	}
	return out
}

func contentResponses() map[string]*huma.Response {
	return map[string]*huma.Response{
		"200": {
			Description: "Document bytes with expected version and blob identity plus a computed digest trailer",
			Headers: map[string]*huma.Param{
				ContentVersionHeader: {Description: "Stable content-version UUID",
					Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}},
				BlobHashHeader: {Description: "Catalog SHA-256 identity (lowercase hex)",
					Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}},
				BlobSizeHeader: {Description: "Catalog raw byte length",
					Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9]+$"}},
				"Content-Digest": {Description: "RFC 9530 SHA-256 of bytes actually streamed; delivered as an HTTP trailer",
					Schema: &huma.Schema{Type: openAPIStringType}},
			},
			Content: map[string]*huma.MediaType{
				"application/octet-stream": {Schema: &huma.Schema{Type: openAPIStringType, Format: openAPIBinaryFormat}},
			},
		},
	}
}

func setContentVersionHeaders(hctx huma.Context, version store.ContentVersion) {
	contentType := version.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hctx.SetHeader("Content-Type", contentType)
	hctx.SetHeader(ContentVersionHeader, version.ID)
	hctx.SetHeader(BlobHashHeader, version.BlobHash)
	hctx.SetHeader(BlobSizeHeader, strconv.FormatInt(version.Size, 10))
	hctx.SetHeader("Trailer", "Content-Digest")
}

func streamContentVersion(
	ctx context.Context, d Deps, version store.ContentVersion,
) (*huma.StreamResponse, error) {
	return streamContentVersionWithFailure(ctx, d, version, false)
}

func streamContentVersionWithFailure(ctx context.Context, d Deps, version store.ContentVersion, abort bool) (*huma.StreamResponse, error) {
	f, _, err := d.Blobs.OpenStreamContext(ctx, version.BlobHash)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal",
			fmt.Sprintf("opening blob %s: %v (run docbank verify)", version.BlobHash, err))
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer func() { _ = f.Close() }()
		setContentVersionHeaders(hctx, version)
		// The blob reader hashes the streamed bytes and verifies this identity
		// at EOF. Reuse that result only after a complete, verified transfer.
		if _, err := io.Copy(hctx.BodyWriter(), f); err == nil && f.Verified() {
			hctx.SetHeader("Content-Digest", contentDigest(mustDecodeHash(version.BlobHash)))
		} else if abort {
			panic(http.ErrAbortHandler)
		}
	}}, nil
}

func streamContentVersionRange(ctx context.Context, d Deps, version store.ContentVersion, requested string) (*huma.StreamResponse, error) {
	start, end, err := parseByteRange(requested, version.Size)
	if err != nil {
		return nil, NewError(http.StatusRequestedRangeNotSatisfiable, "invalid_content_range", "content byte range is invalid or unsatisfiable")
	}
	// Streaming to the offset reads each blob once without materializing it.
	// Seekable opens copy and verify the whole decoded blob first.
	reader, size, err := d.Blobs.OpenStreamContext(ctx, version.BlobHash)
	if err == nil && size == version.Size {
		_, err = io.CopyN(io.Discard, reader, start)
	}
	if err != nil || size != version.Size {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, NewError(http.StatusInternalServerError, "internal", "opening content byte range failed (run docbank verify)")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer func() { _ = reader.Close() }()
		setContentVersionHeaders(hctx, version)
		hctx.SetHeader("Accept-Ranges", "bytes")
		hctx.SetHeader("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, version.Size))
		hctx.SetStatus(http.StatusPartialContent)
		digest := sha256.New()
		if n, err := io.Copy(io.MultiWriter(hctx.BodyWriter(), digest), io.LimitReader(reader, end-start)); err == nil && n == end-start {
			hctx.SetHeader("Content-Digest", contentDigest(digest.Sum(nil)))
		}
	}}, nil
}

func registerReadRoutes(api huma.API, d Deps) {
	rejectEmptyUnderPath := func(ctx huma.Context, next func(huma.Context)) {
		url := ctx.URL()
		if slices.Contains(url.Query()["under_path"], "") {
			if err := huma.WriteErr(api, ctx, http.StatusUnprocessableEntity, "under_path must not be empty"); err != nil {
				d.Logger.DebugContext(ctx.Context(), "write empty search path rejection", "error", err)
			}
			return
		}
		next(ctx)
	}

	huma.Register(api, huma.Operation{
		OperationID: "getNode", Method: http.MethodGet, Path: "/api/v1/nodes/{id}",
		Summary: "Stat a node by id (live or trashed)",
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*nodeOutput, error) {
		return nodeWithPath(ctx, d, in.ID)
	})

	type contentVersionsOutput struct{ Body ContentVersionPage }
	huma.Register(api, huma.Operation{
		OperationID: "listContentVersions", Method: http.MethodGet,
		Path:    "/api/v1/nodes/{id}/versions",
		Summary: "List a file's immutable content versions, newest first",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id"`
		Limit  int   `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*contentVersionsOutput, error) {
		versions, total, err := d.Store.ContentVersions(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &contentVersionsOutput{Body: ContentVersionPage{
			Items: []ContentVersion{}, Total: total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, version := range versions {
			out.Body.Items = append(out.Body.Items, fromStoreContentVersion(version))
		}
		return out, nil
	})

	type contentVersionOutput struct{ Body ContentVersion }
	huma.Register(api, huma.Operation{
		OperationID: "getContentVersion", Method: http.MethodGet,
		Path:    "/api/v1/versions/{version_id}",
		Summary: "Inspect one immutable content version by stable ID",
	}, func(ctx context.Context, in *struct {
		VersionID string `path:"version_id"`
	}) (*contentVersionOutput, error) {
		version, err := d.Store.ContentVersionByID(ctx, in.VersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		body := fromStoreContentVersion(version)
		metadata, metadataErr := d.Store.ContentVersionSourceMetadata(ctx, version.ID)
		if metadataErr == nil {
			body.SourceMetadata = fromStoreSourceMetadata(metadata, browserSessionRequest(ctx))
		}
		if metadataErr != nil && !errors.Is(metadataErr, store.ErrNotFound) &&
			!errors.Is(metadataErr, store.ErrSourceMetadataCorrupt) {
			return nil, FromStoreError(metadataErr)
		}
		return &contentVersionOutput{Body: body}, nil
	})

	versionContentResponses := contentResponses()
	partial := contentResponses()["200"]
	partial.Description = "Selected document byte range with expected version and blob identity plus a computed digest trailer"
	partial.Headers["Content-Range"] = &huma.Param{Schema: &huma.Schema{Type: openAPIStringType}}
	versionContentResponses["206"] = partial
	versionContentResponses["default"] = &huma.Response{Description: "Error", Content: map[string]*huma.MediaType{
		new(Error).ContentType("application/json"): {
			Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, ""),
		},
	}}
	huma.Register(api, huma.Operation{
		OperationID: "getContentVersionBytes", Method: http.MethodGet,
		Path:    "/api/v1/versions/{version_id}/content",
		Summary: "Stream one immutable content version by stable ID",
		Description: "Version, blob hash, and size headers identify expected authority before the body. " +
			"Content-Digest is an RFC 9530 trailer computed from the bytes actually streamed. " +
			"Range selects a single byte window and returns 206 with Content-Range.",
		Responses: versionContentResponses,
	}, func(ctx context.Context, in *struct {
		VersionID string `path:"version_id"`
		Range     string `header:"Range"`
	}) (*huma.StreamResponse, error) {
		version, err := d.Store.ContentVersionByID(ctx, in.VersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if in.Range != "" {
			return streamContentVersionRange(ctx, d, version, in.Range)
		}
		return streamContentVersion(ctx, d, version)
	})

	type contentReferencesOutput struct{ Body ContentReferencePage }
	huma.Register(api, huma.Operation{
		OperationID: "lookupContentReferences", Method: http.MethodGet,
		Path:    "/api/v1/content-references",
		Summary: "Find stable document versions that retain a SHA-256 identity",
		Description: "Results are logical content-version references, not physical loose or pack entries. " +
			"Live current references sort first, followed by live history and trashed references.",
	}, func(ctx context.Context, in *struct {
		SHA256 string `query:"sha256" required:"true" pattern:"^[0-9a-f]{64}$"`
		Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int    `query:"offset" default:"0" minimum:"0"`
	}) (*contentReferencesOutput, error) {
		references, total, err := d.Store.ContentReferencesByHash(
			ctx, in.SHA256, in.Limit, in.Offset,
		)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &contentReferencesOutput{Body: ContentReferencePage{
			Items: []ContentReference{}, Total: total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, ref := range references {
			out.Body.Items = append(out.Body.Items, fromStoreContentReference(ref))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "resolvePath", Method: http.MethodGet, Path: "/api/v1/path",
		Summary: "Resolve an absolute virtual path to its node",
		Description: "path is a query parameter (one well-defined encoding; " +
			"catch-all URL segments are ambiguous for encoded slashes). " +
			"Must start with '/'; '/' resolves the root.",
	}, func(ctx context.Context, in *struct {
		Path string `query:"path" required:"true" example:"/inbox/doc.pdf"`
	}) (*nodeOutput, error) {
		if !strings.HasPrefix(in.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Path))
		}
		view, err := d.Store.NodeSourceMetadataViewByPath(ctx, in.Path)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return nodeOutputFromSourceMetadataView(ctx, view), nil
	})

	type childrenPage struct {
		Body NodePage
	}
	huma.Register(api, huma.Operation{
		OperationID: "listChildren", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/children",
		Summary: "List a directory's live children (dirs first, name-sorted), paginated",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id"`
		Limit  int   `query:"limit" default:"500" minimum:"1" maximum:"5000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*childrenPage, error) {
		page, err := d.Store.DirectoryChildrenPage(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &childrenPage{}
		out.Body.Directory = fromStoreNode(page.Directory.Node)
		out.Body.Directory.Path = page.Directory.Path
		out.Body.Total, out.Body.Limit, out.Body.Offset = page.Total, in.Limit, in.Offset
		out.Body.Items = []Node{}
		for _, child := range page.Children {
			out.Body.Items = append(out.Body.Items, fromStoreNode(child))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getNodeContent", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/content",
		Summary: "Stream a file's bytes",
		Description: "X-Docbank-Content-Version, X-Docbank-Blob-Hash, and X-Docbank-Blob-Size carry catalog identity " +
			"before the body. Content-Digest is an RFC 9530 trailer computed from the bytes " +
			"actually streamed; the standard Content-Length header is omitted so HTTP/1.1 " +
			"can transmit that trailer without a second physical read.",
		Responses: contentResponses(),
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*huma.StreamResponse, error) {
		n, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if n.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", n.ID))
		}
		version, err := d.Store.ContentVersionByID(ctx, n.CurrentVersionID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return streamContentVersion(ctx, d, version)
	})

	type verifyNodeOutput struct{ Body ContentVerification }
	huma.Register(api, huma.Operation{
		OperationID: "verifyNodeContent", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/verify",
		Summary: "Re-hash one file and bind the evidence to its node revision",
		Description: "Requires If-Match from a prior node response. Returns catalog identity and " +
			"a fresh read through the mixed loose/packed store; a concurrent node change returns 412.",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*verifyNodeOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		n, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if n.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", n.ID))
		}
		if n.Revision != revision {
			return nil, staleRevisionError(n.ID, revision, n.Revision)
		}

		report := ContentVerification{
			NodeID: n.ID, VersionID: n.CurrentVersionID, Revision: n.Revision,
			BlobHash: n.BlobHash, Size: n.Size,
		}
		f, _, openErr := d.Blobs.OpenStreamContext(ctx, n.BlobHash)
		if openErr != nil {
			if errors.Is(openErr, fs.ErrNotExist) {
				report.Problem = "missing"
			} else {
				report.Problem = "unreadable"
			}
		} else {
			hash := sha256.New()
			report.ComputedSize, err = io.Copy(hash, f)
			closeErr := f.Close()
			report.ComputedHash = hex.EncodeToString(hash.Sum(nil))
			readErr := errors.Join(err, closeErr)
			if isContentCorruption(readErr) {
				report.Problem = "corrupt"
			} else if readErr != nil {
				report.Problem = "unreadable"
			} else {
				report.Verified = report.ComputedHash == n.BlobHash && report.ComputedSize == n.Size
				if !report.Verified {
					report.Problem = "corrupt"
				}
			}
		}

		current, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, FromStoreError(fmt.Errorf(
					"node %d disappeared during verification: %w", in.ID, store.ErrStaleRevision))
			}
			return nil, FromStoreError(err)
		}
		if current.Revision != revision || current.CurrentVersionID != n.CurrentVersionID {
			return nil, staleRevisionError(n.ID, revision, current.Revision)
		}
		return &verifyNodeOutput{Body: report}, nil
	})

	type textCoverageOutput struct{ Body SearchTextCoverage }
	huma.Register(api, huma.Operation{
		OperationID: "search-text-coverage", Method: http.MethodGet, Path: "/api/v1/search/text-coverage",
		Middlewares: huma.Middlewares{rejectEmptyUnderPath},
		Summary:     "Count files waiting for searchable text",
		Description: "Counts current live files in the search scope that need processing or await native text extraction. Call after an empty text search.",
	}, func(ctx context.Context, in *struct {
		PDFFilter      string `query:"pdf_filter" maxLength:"16384"`
		SourcePresent  bool   `query:"source_present"`
		TagID          string `query:"tag_id" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
		MIMEType       string `query:"mime_type" maxLength:"255"`
		UnderNodeID    int64  `query:"under_node_id" minimum:"1"`
		UnderPath      string `query:"under_path" maxLength:"16384" pattern:"^/"`
		ModifiedSince  string `query:"modified_since" maxLength:"64"`
		ModifiedBefore string `query:"modified_before" maxLength:"64"`
	}) (*textCoverageOutput, error) {
		mimeType, err := store.NormalizeSearchMIMEType(in.MIMEType)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		since, before, err := store.NormalizeSearchTimeBounds(in.ModifiedSince, in.ModifiedBefore)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		opts := searchFilterOptions(&DocumentSourceFenceFilters{SourcePresent: in.SourcePresent, TagID: in.TagID,
			MIMEType: mimeType, UnderNodeID: in.UnderNodeID, UnderPath: in.UnderPath, ModifiedSince: since, ModifiedBefore: before})
		opts.PDFFilter = in.PDFFilter
		profile, noTextLayer := pdfSearchTextCoverage(d)
		var unqueuedPDF func(string, int64) bool
		if d.PDFText != nil {
			unqueuedPDF, err = d.PDFText.UnqueuedPending(ctx)
			if err != nil {
				return nil, FromStoreError(err)
			}
		}
		coverage, err := d.Store.SearchTextCoverage(ctx, opts, profile, noTextLayer, unqueuedPDF)
		if err != nil {
			if _, ok := errors.AsType[*query.ExpressionError](err); ok {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
			}
			return nil, FromStoreError(err)
		}
		return &textCoverageOutput{Body: SearchTextCoverage(coverage)}, nil
	})

	type searchOutput struct{ Body SearchReport }
	huma.Register(api, huma.Operation{
		OperationID: "search", Method: http.MethodGet, Path: "/api/v1/search",
		Middlewares: huma.Middlewares{rejectEmptyUnderPath},
		Summary:     "Search live document names and extracted text",
		Description: "Name matches retain their established BM25 order and appear first; " +
			"content-only matches follow in their own BM25 order. Every hit names its match source. " +
			"The query may be empty when a tag or modification-time bound makes the page bounded; " +
			"such filter-only results are ordered by modification time descending. " +
			"An optional stable tag ID requires that assignment for every result. An optional " +
			"parameter-free MIME type matches the current file version with or without parameters. " +
			"An optional live directory node ID or absolute path restricts results to its descendants; supply only one. Optional " +
			"absolute modification timestamps form an inclusive-since, exclusive-before interval. " +
			"source_present=true excludes documents whose watched source is observed gone; push-owned documents remain included. " +
			"fuzzy=true appends bounded ASCII one-edit filename/path discovery after ordinary " +
			"hits and includes a coverage receipt; it requires a nonblank query, and text " +
			"outside the supplemental syntax reports not_run.",
	}, func(ctx context.Context, in *struct {
		Q              string `query:"q"`
		Fuzzy          bool   `query:"fuzzy"`
		SourcePresent  bool   `query:"source_present"`
		PDFFilter      string `query:"pdf_filter" maxLength:"16384"`
		Limit          int    `query:"limit" default:"50" minimum:"1" maximum:"1000"`
		TagID          string `query:"tag_id" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
		MIMEType       string `query:"mime_type" maxLength:"255"`
		UnderNodeID    int64  `query:"under_node_id" minimum:"1"`
		UnderPath      string `query:"under_path" maxLength:"16384" pattern:"^/"`
		ModifiedSince  string `query:"modified_since" maxLength:"64"`
		ModifiedBefore string `query:"modified_before" maxLength:"64"`
	}) (*searchOutput, error) {
		mimeType, err := store.NormalizeSearchMIMEType(in.MIMEType)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		modifiedSince, modifiedBefore, err := store.NormalizeSearchTimeBounds(
			in.ModifiedSince, in.ModifiedBefore,
		)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		opts := store.SearchOptions{
			PDFFilter:     in.PDFFilter,
			SourcePresent: in.SourcePresent,
			TagID:         in.TagID, MIMEType: mimeType, UnderNodeID: in.UnderNodeID, UnderPath: in.UnderPath,
			ModifiedSince: modifiedSince, ModifiedBefore: modifiedBefore,
		}
		if opts.UnderPath != "" {
			opts, err = d.Store.NormalizeSearchOptions(ctx, opts)
			if err != nil {
				if _, ok := errors.AsType[*query.ExpressionError](err); ok {
					return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
				}
				return nil, FromStoreError(err)
			}
		}
		var hits []store.SearchHit
		var truncated bool
		var fuzzy *FuzzyReceipt
		if in.Fuzzy {
			var receipt store.FuzzyReceipt
			hits, truncated, receipt, err = d.Store.SearchDiscoveryPage(ctx, in.Q, in.Limit, opts)
			fuzzy = &FuzzyReceipt{Status: receipt.Status, Reasons: receipt.Reasons}
		} else {
			hits, truncated, err = d.Store.SearchPageWithOptions(ctx, in.Q, in.Limit, opts)
		}
		if err != nil {
			if _, expressionError := errors.AsType[*query.ExpressionError](err); expressionError || errors.Is(err, store.ErrFuzzyQuery) {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
			}
			return nil, FromStoreError(err)
		}
		out := &searchOutput{Body: SearchReport{
			Hits: []SearchHit{}, Limit: in.Limit, Truncated: truncated, Fuzzy: fuzzy,
			PDFFilter:     in.PDFFilter,
			SourcePresent: in.SourcePresent, TagID: in.TagID, MIMEType: mimeType, UnderNodeID: opts.UnderNodeID, UnderPath: in.UnderPath,
			ModifiedSince: modifiedSince, ModifiedBefore: modifiedBefore,
		}}
		for _, h := range hits {
			out.Body.Hits = append(out.Body.Hits, SearchHit{
				Node: fromStoreNode(h.Node), Path: h.Path, Match: h.Match,
			})
		}
		return out, nil
	})
}
