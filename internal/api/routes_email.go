package api

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

const emailEnsureMaxBodyBytes = 1 << 20

func registerEmailRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerEmailOpenAPI(api)
	registerEmailDocumentRoutes(mux, api, d, g)
	mux.HandleFunc("GET /api/v1/versions/{version_id}/email", func(w http.ResponseWriter, r *http.Request) {
		handleEmailMetadata(w, r, d)
	})
	mux.HandleFunc("POST /api/v1/versions/{version_id}/email", func(w http.ResponseWriter, r *http.Request) {
		handleEnsureEmailMetadata(w, r, d, g)
	})
	mux.HandleFunc("GET /api/v1/versions/{version_id}/email/generations/{generation_id}", func(w http.ResponseWriter, r *http.Request) {
		handleEmailMetadataGeneration(w, r, d)
	})
	mux.HandleFunc("GET /api/v1/versions/{version_id}/email/generations/{generation_id}/parts/{part_path}/{role}", func(w http.ResponseWriter, r *http.Request) {
		handleEmailPart(w, r, d)
	})
}

func handleEmailMetadata(w http.ResponseWriter, r *http.Request, d Deps) {
	view, err := d.Store.EmailMetadata(r.Context(), r.PathValue("version_id"))
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, store.ErrEmailPending) {
		writeJSON(w, http.StatusAccepted, EmailPending{
			Version: fromStoreContentVersion(view.Version), State: "pending",
		})
		return
	}
	if err != nil {
		writeEmailStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fromStoreEmailMetadata(view))
}

func handleEnsureEmailMetadata(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		writeError(w, NewError(http.StatusUnsupportedMediaType, "validation", "email ensure requires application/json"))
		return
	}
	if r.ContentLength > emailEnsureMaxBodyBytes {
		writeError(w, NewError(http.StatusRequestEntityTooLarge, "too_large", "email ensure body exceeds 1 MiB"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, emailEnsureMaxBodyBytes)
	var empty map[string]jsontext.Value
	if err := json.UnmarshalRead(r.Body, &empty); err != nil || empty == nil || len(empty) != 0 {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, NewError(http.StatusRequestEntityTooLarge, "too_large", "email ensure body exceeds 1 MiB"))
			return
		}
		detail := "email ensure body must be an empty JSON object"
		if err != nil {
			detail += ": " + err.Error()
		}
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", detail))
		return
	}
	version, err := d.Store.ContentVersionByID(r.Context(), r.PathValue("version_id"))
	if err != nil {
		writeEmailStoreError(w, err)
		return
	}
	var view store.EmailMetadataView
	err = g.mutate(func() error {
		if d.EnsureEmail == nil {
			return errors.New("email processing is not configured")
		}
		var ensureErr error
		view, ensureErr = d.EnsureEmail(
			r.Context(), d.Store, d.Blobs, (home.Layout{Root: d.VaultRoot}).BlobTmpDir(),
			store.EmailTarget{Version: version},
		)
		return ensureErr
	})
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		writeEmailStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fromStoreEmailMetadata(view))
}

func handleEmailMetadataGeneration(w http.ResponseWriter, r *http.Request, d Deps) {
	view, err := d.Store.EmailMetadataGeneration(
		r.Context(), r.PathValue("version_id"), r.PathValue("generation_id"),
	)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		writeEmailStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fromStoreEmailMetadata(view))
}

func handleEmailPart(w http.ResponseWriter, r *http.Request, d Deps) {
	ctx := r.Context()
	versionID, generationID := r.PathValue("version_id"), r.PathValue("generation_id")
	partPath, role := r.PathValue("part_path"), r.PathValue("role")
	if err := document.ValidateEmailPartPath(partPath); err != nil || !emailArtifactRole(role) {
		writeEmailStoreError(w, store.ErrInvalidEmailPart)
		return
	}
	receipt, err := d.Store.EmailPart(ctx, versionID, generationID, partPath, role)
	if err != nil {
		writeEmailStoreError(w, err)
		return
	}
	reader, physicalSize, err := d.Blobs.OpenStreamContext(ctx, receipt.BlobSHA256)
	if err != nil {
		writeError(w, NewError(http.StatusInternalServerError, "content_corrupt",
			fmt.Sprintf("opening email part %s/%s: %v (run docbank verify)", partPath, role, err)))
		return
	}
	if physicalSize != receipt.Size {
		_ = reader.Close()
		writeError(w, NewError(http.StatusInternalServerError, "content_corrupt", fmt.Sprintf(
			"email part catalog size %d does not match physical size %d", receipt.Size, physicalSize)))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": receipt.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(ContentVersionHeader, receipt.Version.ID)
	w.Header().Set(EmailGenerationHeader, receipt.GenerationID)
	w.Header().Set(EmailAttachmentHeader, receipt.AttachmentID)
	w.Header().Set(EmailPartPathHeader, receipt.PartPath)
	w.Header().Set(EmailPartRoleHeader, receipt.Role)
	w.Header().Set(BlobHashHeader, receipt.BlobSHA256)
	w.Header().Set(BlobSizeHeader, strconv.FormatInt(receipt.Size, 10))
	w.Header().Set("Trailer", "Content-Digest")
	hash := sha256.New()
	written, copyErr := io.Copy(w, io.TeeReader(reader, hash))
	closeErr := reader.Close()
	if copyErr == nil && closeErr == nil && written == receipt.Size {
		w.Header().Set("Content-Digest", contentDigest(hash.Sum(nil)))
	}
}

func emailArtifactRole(role string) bool {
	return document.IsEmailArtifactRole(role)
}

func writeEmailStoreError(w http.ResponseWriter, err error) {
	var response *Error
	if errors.As(err, &response) {
		writeError(w, response)
		return
	}
	mapped := FromStoreError(err)
	if errors.As(mapped, &response) {
		writeError(w, response)
		return
	}
	writeError(w, NewError(http.StatusInternalServerError, "internal", mapped.Error()))
}

func registerEmailOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	metadata := huma.SchemaFromType(registry, reflect.TypeFor[EmailMetadata]())
	pending := huma.SchemaFromType(registry, reflect.TypeFor[EmailPending]())
	errorSchema := huma.SchemaFromType(registry, reflect.TypeFor[Error]())
	jsonResponse := func(description string, schema *huma.Schema) *huma.Response {
		return &huma.Response{Description: description, Headers: map[string]*huma.Param{
			"Cache-Control": {Description: "Mutable selection state is not cacheable", Schema: &huma.Schema{Type: openAPIStringType}},
		}, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	}
	defaultResponse := &huma.Response{Description: "Error", Content: map[string]*huma.MediaType{
		"application/problem+json": {Schema: errorSchema},
	}}
	artifactRoles := document.EmailArtifactRoles()
	artifactRoleEnum := make([]any, 0, len(artifactRoles))
	for _, role := range artifactRoles {
		artifactRoleEnum = append(artifactRoleEnum, string(role))
	}
	versionParam := &huma.Param{Name: "version_id", In: "path", Required: true, Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}
	generationParam := &huma.Param{Name: "generation_id", In: "path", Required: true, Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}}
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "getEmailMetadata", Method: http.MethodGet,
		Path: "/api/v1/versions/{version_id}/email", Summary: "Read selected email metadata for one immutable version",
		Parameters: []*huma.Param{versionParam}, Responses: map[string]*huma.Response{
			"200": jsonResponse("Selected email metadata", metadata),
			"202": jsonResponse("Eligible email metadata is pending", pending), "default": defaultResponse,
		},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "ensureEmailMetadata", Method: http.MethodPost,
		Path: "/api/v1/versions/{version_id}/email", Summary: "Ensure email metadata for one immutable version",
		Parameters: []*huma.Param{versionParam}, RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"application/json": {Schema: &huma.Schema{Type: "object", AdditionalProperties: false}},
		}}, Responses: map[string]*huma.Response{"200": jsonResponse("Email metadata", metadata), "default": defaultResponse},
	})
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "getEmailMetadataGeneration", Method: http.MethodGet,
		Path:       "/api/v1/versions/{version_id}/email/generations/{generation_id}",
		Summary:    "Read an immutable email generation attached to one version",
		Parameters: []*huma.Param{versionParam, generationParam},
		Responses:  map[string]*huma.Response{"200": jsonResponse("Attached email generation", metadata), "default": defaultResponse},
	})
	partResponses := contentResponses()
	partResponses["200"].Description = "Verified bytes for one exact email part artifact"
	partResponses["200"].Headers[EmailGenerationHeader] = &huma.Param{Description: "Immutable email generation SHA-256", Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}}
	partResponses["200"].Headers[EmailAttachmentHeader] = &huma.Param{Description: "Version-to-generation attachment SHA-256", Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{64}$"}}
	partResponses["200"].Headers[EmailPartPathHeader] = &huma.Param{Description: "Canonical dotted MIME part path", Schema: &huma.Schema{Type: openAPIStringType}}
	partResponses["200"].Headers[EmailPartRoleHeader] = &huma.Param{Description: "Typed MIME artifact role", Schema: &huma.Schema{Type: openAPIStringType, Enum: artifactRoleEnum}}
	partResponses["default"] = defaultResponse
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "getEmailPart", Method: http.MethodGet,
		Path:    "/api/v1/versions/{version_id}/email/generations/{generation_id}/parts/{part_path}/{role}",
		Summary: "Stream one verified immutable email part artifact",
		Parameters: []*huma.Param{versionParam, generationParam,
			{Name: "part_path", In: "path", Required: true, Schema: &huma.Schema{Type: openAPIStringType}},
			{Name: "role", In: "path", Required: true, Schema: &huma.Schema{Type: openAPIStringType, Enum: artifactRoleEnum}},
		}, Responses: partResponses,
	})
}
