package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

const maxPackageRequestBytes = 1 << 20
const maxPackageDiagnosticSummary = 250
const maxPackageRecords = 100_000
const maxPackagePages = 1_000_000
const maxPackageNormalizedMemory = int64(256 << 20)

func registerPackageRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerPackageOpenAPI(api)
	registerPackageBrowseRoutes(api, d)
	registerPackageCustodianRoutes(mux, d, g)
	registerPackageContainerRoutes(mux, d, g)
	mux.HandleFunc("POST /api/v1/packages/containers/{id}/preflight", func(w http.ResponseWriter, r *http.Request) {
		handlePackageContainerPreflight(w, r, d, g)
	})
	mux.HandleFunc("POST /api/v1/packages/preflights", func(w http.ResponseWriter, r *http.Request) {
		handlePackagePreflight(w, r, d, g)
	})
	mux.HandleFunc("POST /api/v1/packages/imports", func(w http.ResponseWriter, r *http.Request) {
		handlePackageImport(w, r, d, g)
	})
	mux.HandleFunc("GET /api/v1/packages/imports/{operation_id}", func(w http.ResponseWriter, r *http.Request) {
		handlePackageImportStatus(w, r, d)
	})
	mux.HandleFunc("POST /api/v1/packages/imports/{operation_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		handlePackageImportCancel(w, r, d, g)
	})
	mux.HandleFunc("GET /api/v1/packages/preflights/{preflight_id}", func(w http.ResponseWriter, r *http.Request) {
		owner, problem := packageContainerOwner(r, d)
		if problem != nil {
			writeError(w, problem)
			return
		}
		record, err := d.Store.PackagePreflight(r.Context(), owner, r.PathValue("preflight_id"))
		if err != nil {
			writePackageError(w, err)
			return
		}
		out, err := packagePreflightFromRecord(record)
		if err != nil {
			writePackageError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/packages/preflights/{preflight_id}/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		handlePackageDiagnostics(w, r, d)
	})
}

func registerPackageOpenAPI(api huma.API) {
	registerPackageContainerOpenAPI(api)
	registerPackageCustodianOpenAPI(api)
	registry := api.OpenAPI().Components.Schemas
	request := registry.Schema(reflect.TypeFor[PackagePreflightRequest](), true, "")
	preflight := &huma.Response{Description: "Expiring package preview", Content: map[string]*huma.MediaType{
		jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackagePreflight](), true, "")},
	}}
	diagnostics := &huma.Response{Description: "Page of package diagnostics", Content: map[string]*huma.MediaType{
		jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackageDiagnosticPage](), true, "")},
	}}
	errorResponse := &huma.Response{Description: "Error", Content: map[string]*huma.MediaType{
		"application/problem+json": {Schema: registry.Schema(reflect.TypeFor[Error](), true, "")},
	}}
	preflightID := &huma.Param{Name: "preflight_id", In: openAPIPathLocation, Required: true,
		Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}
	operationID := &huma.Param{Name: "operation_id", In: openAPIPathLocation, Required: true,
		Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}
	importJob := &huma.Response{Description: "Durable package import job", Content: map[string]*huma.MediaType{
		jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackageImportJob](), true, "")},
	}}
	for _, operation := range []*huma.Operation{
		{OperationID: "createPackageImport", Method: http.MethodPost, Path: "/api/v1/packages/imports",
			Summary: "Import a previewed load-file package", MaxBodyBytes: maxPackageRequestBytes,
			RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
				jsonMediaType: {Schema: registry.Schema(reflect.TypeFor[PackageImportRequest](), true, "")},
			}}, Responses: map[string]*huma.Response{"202": importJob, "404": errorResponse, "409": errorResponse, "422": errorResponse}},
		{OperationID: "readPackageImport", Method: http.MethodGet, Path: "/api/v1/packages/imports/{operation_id}",
			Summary: "Read package import progress", Parameters: []*huma.Param{operationID},
			Responses: map[string]*huma.Response{"200": importJob, "404": errorResponse}},
		{OperationID: "cancelPackageImport", Method: http.MethodPost, Path: "/api/v1/packages/imports/{operation_id}/cancel",
			Summary: "Cancel a package import", Parameters: []*huma.Param{operationID},
			Responses: map[string]*huma.Response{"200": importJob, "404": errorResponse, "409": errorResponse}},
		{OperationID: "createPackagePreflight", Method: http.MethodPost, Path: "/api/v1/packages/preflights",
			Summary: "Preview a load-file package", MaxBodyBytes: maxPackageRequestBytes,
			RequestBody: &huma.RequestBody{Required: true, Description: "Package source and profiles, limited to 1 MiB of JSON", Content: map[string]*huma.MediaType{
				jsonMediaType: {Schema: request},
			}},
			Responses: map[string]*huma.Response{"200": preflight, "413": errorResponse, "422": errorResponse, "503": errorResponse}},
		{OperationID: "readPackagePreflight", Method: http.MethodGet, Path: "/api/v1/packages/preflights/{preflight_id}",
			Summary: "Read an expiring package preview", Parameters: []*huma.Param{preflightID},
			Responses: map[string]*huma.Response{"200": preflight, "404": errorResponse}},
		{OperationID: "readPackagePreflightDiagnostics", Method: http.MethodGet, Path: "/api/v1/packages/preflights/{preflight_id}/diagnostics",
			Summary: "Read one bounded page of package diagnostics",
			Parameters: []*huma.Param{
				preflightID,
				{Name: "limit", In: openAPIQueryLocation, Description: "Maximum diagnostics to return",
					Schema: &huma.Schema{Type: "integer", Default: 100, Minimum: new(float64(1)), Maximum: new(float64(maxPackageDiagnosticSummary))}},
				{Name: "cursor", In: openAPIQueryLocation, Description: "Opaque next_cursor returned by this preflight's previous diagnostic page",
					Schema: &huma.Schema{Type: openAPIStringType}},
			},
			Responses: map[string]*huma.Response{"200": diagnostics, "404": errorResponse, "422": errorResponse, "503": errorResponse}},
	} {
		for _, status := range []string{"401", "403", "500"} {
			operation.Responses[status] = errorResponse
		}
		api.OpenAPI().AddOperation(operation)
	}
}

func handlePackageDiagnostics(w http.ResponseWriter, r *http.Request, d Deps) {
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	record, err := d.Store.PackagePreflight(r.Context(), owner, r.PathValue("preflight_id"))
	if err != nil {
		writePackageError(w, err)
		return
	}
	preflight, err := packagePreflightFromRecord(record)
	if err != nil {
		writePackageError(w, err)
		return
	}
	limit, offset, err := packageDiagnosticPageInput(r, preflight.PreflightID)
	if err != nil {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", err.Error()))
		return
	}
	reader, err := d.Blobs.OpenContext(r.Context(), record.DiagnosticsBlobSHA256)
	if err != nil {
		writePackageError(w, err)
		return
	}
	defer func() { _ = reader.Close() }()
	page := PackageDiagnosticPage{Diagnostics: make([]PackageDiagnostic, 0, limit), Total: preflight.DiagnosticCount}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8*loadfile.MaxFieldValueBytes+4096)
	ordinal := 0
	for scanner.Scan() {
		if ordinal >= offset && len(page.Diagnostics) < limit {
			var diagnostic loadfile.Diagnostic
			if err := json.Unmarshal(scanner.Bytes(), &diagnostic); err != nil {
				writePackageError(w, fmt.Errorf("decode retained package diagnostic: %w", err))
				return
			}
			page.Diagnostics = append(page.Diagnostics, packageDiagnostics([]loadfile.Diagnostic{diagnostic})[0])
		}
		ordinal++
	}
	if err := scanner.Err(); err != nil {
		writePackageError(w, fmt.Errorf("read retained package diagnostics: %w", err))
		return
	}
	if offset+len(page.Diagnostics) < page.Total {
		page.NextCursor = packageDiagnosticCursor(preflight.PreflightID, offset+len(page.Diagnostics))
	}
	writeJSON(w, http.StatusOK, page)
}

func packageDiagnosticPageInput(r *http.Request, preflightID string) (int, int, error) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxPackageDiagnosticSummary {
			return 0, 0, fmt.Errorf("diagnostic limit must be between 1 and %d", maxPackageDiagnosticSummary)
		}
		limit = parsed
	}
	rawCursor := r.URL.Query().Get("cursor")
	if rawCursor == "" {
		return limit, 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawCursor)
	if err != nil {
		return 0, 0, errors.New("diagnostic cursor is invalid")
	}
	prefix := preflightID + ":"
	if !strings.HasPrefix(string(decoded), prefix) {
		return 0, 0, errors.New("diagnostic cursor does not belong to this preflight")
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(string(decoded), prefix))
	if err != nil || offset < 0 {
		return 0, 0, errors.New("diagnostic cursor is invalid")
	}
	return limit, offset, nil
}

func packageDiagnosticCursor(preflightID string, offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(preflightID + ":" + strconv.Itoa(offset)))
}

func handlePackagePreflight(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	var request PackagePreflightRequest
	if err := readPackageJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	if request.SourceKind != "root" {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "source_kind must be root"))
		return
	}
	if request.SourceRef == "" {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "source_ref must name a package directory"))
		return
	}
	result, err := buildPackagePreflight(r.Context(), d, g, owner, request)
	if err != nil {
		writePackageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func readPackageJSON(w http.ResponseWriter, r *http.Request, target any) *Error {
	r.Body = http.MaxBytesReader(w, r.Body, maxPackageRequestBytes)
	if err := json.UnmarshalRead(r.Body, target, json.RejectUnknownMembers(true)); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return NewError(http.StatusRequestEntityTooLarge, "too_large", "package request exceeds 1 MiB")
		}
		return NewError(http.StatusUnprocessableEntity, "validation", "invalid package request: "+err.Error())
	}
	return nil
}

func buildPackagePreflight(ctx context.Context, d Deps, g *gate, owner string, request PackagePreflightRequest) (PackagePreflight, error) {
	return buildPackagePreflightFromRoot(ctx, d, g, owner, request, request.SourceRef, request.SourceRef)
}

// PreflightPackage runs the shared package preflight without an HTTP transport.
// Embedded vaults pass their own mutation gate so physical and metadata writes
// remain coordinated with every other in-process operation.
func PreflightPackage(ctx context.Context, d Deps, g PackageMutationGate, owner string, request PackagePreflightRequest) (PackagePreflight, error) {
	if request.SourceKind != "root" || request.SourceRef == "" {
		return PackagePreflight{}, store.ErrPackageConflict
	}
	return buildPackagePreflightFromRoot(ctx, d, g, owner, request, request.SourceRef, request.SourceRef)
}

// ReadPackagePreflight returns one owner-scoped retained preflight.
func ReadPackagePreflight(ctx context.Context, catalog *store.Store, owner, preflightID string) (PackagePreflight, error) {
	record, err := catalog.PackagePreflight(ctx, owner, preflightID)
	if err != nil {
		return PackagePreflight{}, err
	}
	return packagePreflightFromRecord(record)
}

func buildPackagePreflightFromRoot(ctx context.Context, d Deps, g PackageMutationGate, owner string, request PackagePreflightRequest, sourceRoot, sourceLocator string) (PackagePreflight, error) {
	profile, err := loadfile.ReadProfile(request.Profile)
	if err != nil {
		return PackagePreflight{}, err
	}
	profile.Encoding = request.Encoding
	if _, err := loadfile.Decoder(profile.Encoding); err != nil {
		return PackagePreflight{}, err
	}
	resolver, err := loadfile.NewResolver(ctx, sourceRoot, nil)
	if err != nil {
		return PackagePreflight{}, err
	}
	defer func() { _ = resolver.Close() }()
	if err := resolver.EnforceMaxFileBytes(blob.MaxIngestBytes); err != nil {
		return PackagePreflight{}, err
	}
	if request.SourceKind == "root" {
		sourceLocator = resolver.Root
	}
	datName, pageMapName, discoveredVolumes, err := resolver.DiscoverPackageFiles()
	if err != nil {
		return PackagePreflight{}, err
	}
	if pageMapName == "" && request.PageMapProfile != "" {
		return PackagePreflight{}, fmt.Errorf("%w: page_map_profile requires an OPT or LFP page map", loadfile.ErrInvalidProfile)
	}
	datPath := filepath.Join(resolver.Root, filepath.FromSlash(datName))
	pageMapPath := ""
	if pageMapName != "" {
		pageMapPath = filepath.Join(resolver.Root, filepath.FromSlash(pageMapName))
	}
	datVolume, datRel, err := packagePathReference(resolver.Root, datPath, discoveredVolumes, nil)
	if err != nil {
		return PackagePreflight{}, err
	}
	dat, err := resolver.Open(datVolume, datRel)
	if err != nil {
		return PackagePreflight{}, err
	}
	records := make([]loadfile.Record, 0, 100)
	memoryBudget := packageMemoryBudget{maximum: maxPackageNormalizedMemory}
	scan := loadfile.ScanDAT
	if profile.ID == "csv-rfc4180-v1" {
		scan = loadfile.ScanCSV
	}
	datDigest := sha256.New()
	diagnostics, parseErr := scan(io.TeeReader(dat, datDigest), profile, func(record loadfile.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(records) == maxPackageRecords {
			return loadfile.ErrLoadfileLimit
		}
		record.LoadFile = filepath.Base(datPath)
		if err := memoryBudget.add(packageRecordMemory(record)); err != nil {
			return err
		}
		records = append(records, record)
		return nil
	})
	datSize, positionErr := dat.Seek(0, io.SeekCurrent)
	closeErr := dat.Close()
	if err := errors.Join(parseErr, positionErr, closeErr); err != nil {
		return PackagePreflight{}, err
	}
	if datSize != dat.Size() {
		return PackagePreflight{}, fmt.Errorf("%w: metadata load file size changed during preflight", loadfile.ErrMalformedInput)
	}
	mapping := loadfile.Mapping{Contract: loadfile.MappingContractV1}
	mappingSHA, err := packageMappingSHA256(mapping)
	if err != nil {
		return PackagePreflight{}, err
	}
	if len(request.Mapping) > 0 {
		var columns []string
		if len(records) > 0 {
			columns = records[0].ColumnOrder
		}
		mapping, mappingSHA, err = loadfile.DecodeMapping(request.Mapping, columns)
		if err != nil {
			return PackagePreflight{}, err
		}
	}
	mappedDiagnostics, err := loadfile.ApplyMapping(records, mapping, profile, memoryBudget.add)
	if err != nil {
		return PackagePreflight{}, err
	}
	diagnostics = append(diagnostics, mappedDiagnostics...)
	if err := resolver.SetVolumeRoots(mapping.VolumeRoots); err != nil {
		return PackagePreflight{}, err
	}
	volumes, err := logicalPackageVolumes(discoveredVolumes, mapping)
	if err != nil {
		return PackagePreflight{}, err
	}
	loadfile.NormalizeFileReferences(records, volumes)
	for _, record := range records {
		encoded, err := canonical.Marshal(record)
		if err != nil {
			return PackagePreflight{}, err
		}
		if len(encoded) > 1<<20 {
			return PackagePreflight{}, loadfile.ErrLoadfileLimit
		}
	}
	if err := memoryBudget.reset(packageRecordsMemory(records)); err != nil {
		return PackagePreflight{}, err
	}
	images := []loadfile.ImageRef{}
	var pageMapFile loadfile.FileRef
	if pageMapPath != "" {
		pageMapVolume, pageMapRel, pathErr := packagePathReference(resolver.Root, pageMapPath, volumes, mapping.VolumeRoots)
		if pathErr != nil {
			return PackagePreflight{}, pathErr
		}
		mapProfileID := request.PageMapProfile
		isLFP := strings.EqualFold(filepath.Ext(pageMapPath), ".lfp")
		if mapProfileID == "" {
			mapProfileID = "opt-standard-v1"
			if isLFP {
				mapProfileID = "lfp-ipro-v1"
			}
		}
		if (isLFP && mapProfileID != "lfp-ipro-v1") || (!isLFP && mapProfileID != "opt-standard-v1" && mapProfileID != "opt-pagecount5-v1") {
			return PackagePreflight{}, fmt.Errorf("%w: page_map_profile must name a supported profile for the page map format", loadfile.ErrInvalidProfile)
		}
		pageMapProfile, profileErr := loadfile.ReadProfile(mapProfileID)
		if profileErr != nil {
			return PackagePreflight{}, profileErr
		}
		pageMapProfile.Encoding = request.Encoding
		opt, openErr := resolver.Open(pageMapVolume, pageMapRel)
		if openErr != nil {
			return PackagePreflight{}, openErr
		}
		pageMapDigest := sha256.New()
		pageMapSource := io.TeeReader(opt, pageMapDigest)
		var optDiagnostics []loadfile.Diagnostic
		visit := func(image loadfile.ImageRef) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(images) == maxPackagePages {
				return loadfile.ErrLoadfileLimit
			}
			if err := memoryBudget.add(packageImageMemory(image)); err != nil {
				return err
			}
			images = append(images, image)
			return nil
		}
		if mapProfileID == "lfp-ipro-v1" {
			parseErr = loadfile.ScanLFP(ctx, pageMapSource, pageMapProfile, visit)
		} else {
			optDiagnostics, parseErr = loadfile.ScanOPT(ctx, pageMapSource, pageMapProfile, visit)
		}
		pageMapSize, positionErr := opt.Seek(0, io.SeekCurrent)
		closeErr = opt.Close()
		diagnostics = append(diagnostics, optDiagnostics...)
		if err := errors.Join(parseErr, positionErr, closeErr); err != nil {
			return PackagePreflight{}, err
		}
		if pageMapSize != opt.Size() {
			return PackagePreflight{}, fmt.Errorf("%w: page map size changed during preflight", loadfile.ErrMalformedInput)
		}
		pageMapFile = loadfile.FileRef{Role: "raw_load_file", Volume: pageMapVolume.Name, RelPath: pageMapRel, Declared: pageMapRel,
			SHA256: hex.EncodeToString(pageMapDigest.Sum(nil)), Size: pageMapSize, Status: "available"}
	}
	loadFileCount := 1
	if pageMapPath != "" {
		loadFileCount++
	}
	if err := memoryBudget.add(packageManifestFilesMemory(records, images, loadFileCount)); err != nil {
		return PackagePreflight{}, err
	}
	files, validated, err := loadfile.Validate(ctx, loadfile.ValidateInput{Records: records, Images: images, Volumes: volumes, Resolver: resolver, PageCount: func(file io.ReadSeeker) (int, error) {
		return pdfapi.PageCount(file, nil)
	}})
	if err != nil {
		return PackagePreflight{}, err
	}
	diagnostics = append(diagnostics, validated...)
	profileSHA, err := profile.SHA256()
	if err != nil {
		return PackagePreflight{}, err
	}
	datVolume, datRel, err = packagePathReference(resolver.Root, datPath, volumes, mapping.VolumeRoots)
	if err != nil {
		return PackagePreflight{}, err
	}
	files = append(files, loadfile.FileRef{Role: "raw_load_file", Volume: datVolume.Name, RelPath: datRel, Declared: datRel,
		SHA256: hex.EncodeToString(datDigest.Sum(nil)), Size: datSize, Status: "available"})
	if pageMapPath != "" {
		files = append(files, pageMapFile)
	}
	manifest := loadfile.Manifest{ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, Mapping: mapping, Volumes: volumes, Records: records, Images: images, Files: files}
	manifestSHA, err := manifest.SHA256()
	if err != nil {
		return PackagePreflight{}, err
	}
	rootDigest, err := resolver.RootDigest()
	if err != nil {
		return PackagePreflight{}, err
	}
	out := PackagePreflight{PreflightID: uuid.NewString(), SourceKind: request.SourceKind, SourceRef: rootDigest, ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, ManifestSHA256: manifestSHA, Records: len(records), Pages: len(images), Blocking: loadfile.Blocking(diagnostics), Diagnostics: packageDiagnostics(diagnostics)}
	for _, volume := range volumes {
		out.Volumes = append(out.Volumes, PackageVolume{Ordinal: volume.Ordinal, VolumeName: volume.Name, DeclaredRoot: volume.DeclaredRoot})
	}
	var stored PackagePreflight
	err = g.MutateContext(ctx, func() error {
		var persistErr error
		stored, persistErr = persistPackagePreflight(ctx, d, owner, sourceLocator, profile, manifest, diagnostics, out)
		return persistErr
	})
	return stored, err
}

type packageMemoryBudget struct {
	used    int64
	maximum int64
}

func (b *packageMemoryBudget) add(size int64) error {
	if size < 0 || size > b.maximum-b.used {
		return loadfile.ErrLoadfileLimit
	}
	b.used += size
	return nil
}

func (b *packageMemoryBudget) reset(size int64) error {
	b.used = 0
	return b.add(size)
}

func packageRecordsMemory(records []loadfile.Record) int64 {
	var size int64
	for _, record := range records {
		size += packageRecordMemory(record)
	}
	return size
}

func packageRecordMemory(record loadfile.Record) int64 {
	size := int64(256 + len(record.RowID) + len(record.DocID) + len(record.LoadFile))
	for _, column := range record.ColumnOrder {
		size += int64(16 + len(column))
	}
	for _, field := range record.Fields {
		size += int64(192 + len(field.Column) + len(field.Canonical) + len(field.Raw) + len(field.Value.Kind) + len(field.Value.Text))
		for _, item := range field.Value.List {
			size += int64(16 + len(item))
		}
		if field.Value.Time != nil {
			size += int64(128 + len(field.Value.Time.Raw) + len(field.Value.Time.DateValue) + len(field.Value.Time.Precision) + len(field.Value.Time.TimezoneKind) + len(field.Value.Time.ZoneText))
		}
	}
	for _, file := range record.Files {
		size += int64(256 + len(file.Role) + len(file.Volume) + len(file.RelPath) + len(file.Declared) + len(file.SHA256) + len(file.Status))
	}
	size += int64(len(record.Family.ParentDocID) + len(record.Family.RangeBegin) + len(record.Family.RangeEnd) + len(record.Family.GroupID))
	for _, child := range record.Family.AttachmentDocIDs {
		size += int64(16 + len(child))
	}
	return size
}

func packageImageMemory(image loadfile.ImageRef) int64 {
	return int64(192 + len(image.ImageKey) + len(image.Volume) + len(image.RelPath) + len(image.Boundary))
}

func packageManifestFilesMemory(records []loadfile.Record, images []loadfile.ImageRef, loadFileCount int) int64 {
	size := int64(loadFileCount * 512)
	for _, record := range records {
		for _, file := range record.Files {
			size += int64(320 + len(file.Role) + len(file.Volume) + len(file.RelPath) + len(file.Declared) + len(file.Status))
		}
	}
	for _, image := range images {
		size += int64(320 + len(image.Volume) + 2*len(image.RelPath))
	}
	return size
}

func persistPackagePreflight(ctx context.Context, d Deps, owner, sourceLocator string, profile loadfile.Profile, manifest loadfile.Manifest, diagnostics []loadfile.Diagnostic, out PackagePreflight) (PackagePreflight, error) {
	profileJSON, err := canonical.Marshal(profile)
	if err != nil {
		return PackagePreflight{}, err
	}
	mappingJSON, err := canonical.Marshal(manifest.Mapping)
	if err != nil {
		return PackagePreflight{}, err
	}
	manifestFile, err := os.CreateTemp(d.VaultRoot, ".package-manifest-*")
	if err != nil {
		return PackagePreflight{}, fmt.Errorf("create package manifest staging file: %w", err)
	}
	defer func() {
		_ = manifestFile.Close()
		_ = os.Remove(manifestFile.Name())
	}()
	if err := manifest.WriteJSONL(manifestFile); err != nil {
		return PackagePreflight{}, err
	}
	if _, err := manifestFile.Seek(0, io.SeekStart); err != nil {
		return PackagePreflight{}, fmt.Errorf("rewind package manifest staging file: %w", err)
	}
	diagnosticsFile, err := os.CreateTemp(d.VaultRoot, ".package-diagnostics-*")
	if err != nil {
		return PackagePreflight{}, fmt.Errorf("create package diagnostics staging file: %w", err)
	}
	defer func() {
		_ = diagnosticsFile.Close()
		_ = os.Remove(diagnosticsFile.Name())
	}()
	if err := writePackageDiagnosticsJSONL(diagnosticsFile, diagnostics); err != nil {
		return PackagePreflight{}, err
	}
	if _, err := diagnosticsFile.Seek(0, io.SeekStart); err != nil {
		return PackagePreflight{}, fmt.Errorf("rewind package diagnostics staging file: %w", err)
	}
	out.DiagnosticCount = len(diagnostics)
	summaryBytes, err := canonical.Marshal(out)
	if err != nil {
		return PackagePreflight{}, err
	}
	diagnosticSummary, err := canonical.Marshal(packageDiagnostics(diagnostics))
	if err != nil {
		return PackagePreflight{}, err
	}
	record := store.PackagePreflightRecord{
		PreflightID: out.PreflightID, Owner: owner, SourceKind: out.SourceKind,
		SourceRef: out.SourceRef, SourceLocator: sourceLocator,
		ProfileSHA256: out.ProfileSHA256, ProfileJSON: string(profileJSON),
		MappingSHA256: out.MappingSHA256, MappingJSON: string(mappingJSON),
		ManifestSHA256: out.ManifestSHA256, CanonicalJSON: summaryBytes,
		DiagnosticsJSON: diagnosticSummary, Blocking: out.Blocking,
	}
	err = d.Blobs.WithMutation(ctx, func() error {
		manifestReceipt, writeErr := d.Blobs.WriteDetailedContext(ctx, manifestFile)
		if writeErr != nil {
			return writeErr
		}
		if manifestReceipt.Hash != out.ManifestSHA256 {
			return fmt.Errorf("stored package manifest digest %s does not match normalized manifest %s", manifestReceipt.Hash, out.ManifestSHA256)
		}
		if writeErr = recordPackagePreflightBlob(ctx, d.Store, manifestReceipt); writeErr != nil {
			return writeErr
		}
		record.ManifestBlobSHA256 = manifestReceipt.Hash
		diagnosticsReceipt, writeErr := d.Blobs.WriteDetailedContext(ctx, diagnosticsFile)
		if writeErr != nil {
			return writeErr
		}
		if writeErr = recordPackagePreflightBlob(ctx, d.Store, diagnosticsReceipt); writeErr != nil {
			return writeErr
		}
		record.DiagnosticsBlobSHA256 = diagnosticsReceipt.Hash
		stored, putErr := d.Store.PutPackagePreflight(ctx, record)
		if putErr != nil {
			return putErr
		}
		out.CreatedAt, out.ExpiresAt = stored.CreatedAt, stored.ExpiresAt
		return nil
	})
	return out, err
}

func writePackageDiagnosticsJSONL(writer io.Writer, diagnostics []loadfile.Diagnostic) error {
	for _, diagnostic := range diagnostics {
		encoded, err := canonical.Marshal(diagnostic)
		if err != nil {
			return fmt.Errorf("encode package diagnostic: %w", err)
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			return fmt.Errorf("write package diagnostic: %w", err)
		}
	}
	return nil
}

func recordPackagePreflightBlob(ctx context.Context, catalog *store.Store, receipt blob.WriteReceipt) error {
	encoding, err := receipt.EncodingName()
	if err != nil {
		return err
	}
	return catalog.RecordRenditionBlob(ctx, receipt.Hash, receipt.Size, store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, MD5: receipt.MD5, Created: receipt.Created})
}

func logicalPackageVolumes(discovered []loadfile.Volume, mapping loadfile.Mapping) ([]loadfile.Volume, error) {
	if len(mapping.VolumeRoots) == 0 {
		return discovered, nil
	}
	for _, volume := range discovered {
		covered := false
		for _, root := range mapping.VolumeRoots {
			first, _, _ := strings.Cut(strings.ReplaceAll(root, `\`, "/"), "/")
			if first == volume.DeclaredRoot {
				covered = true
				break
			}
		}
		if !covered {
			return nil, fmt.Errorf("%w: volume_roots must cover discovered volume %q", loadfile.ErrInvalidMapping, volume.Name)
		}
	}
	names := make([]string, 0, len(mapping.VolumeRoots))
	for name := range mapping.VolumeRoots {
		names = append(names, name)
	}
	slices.Sort(names)
	volumes := make([]loadfile.Volume, len(names))
	for i, name := range names {
		volumes[i] = loadfile.Volume{Name: name, DeclaredRoot: name, Ordinal: i + 1}
	}
	return volumes, nil
}

func packagePathReference(root, path string, volumes []loadfile.Volume, volumeRoots map[string]string) (loadfile.Volume, string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return loadfile.Volume{}, "", loadfile.ErrUnsafeReference
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return loadfile.Volume{}, "", loadfile.ErrUnsafeReference
	}
	bestRoot := ""
	var best loadfile.Volume
	for _, volume := range volumes {
		declaredRoot := volume.DeclaredRoot
		if mapped, ok := volumeRoots[volume.Name]; ok {
			declaredRoot = strings.ReplaceAll(mapped, `\`, "/")
		}
		declaredRoot = strings.TrimSuffix(declaredRoot, "/")
		if strings.HasPrefix(rel, declaredRoot+"/") && len(declaredRoot) > len(bestRoot) {
			bestRoot, best = declaredRoot, volume
		}
	}
	if bestRoot != "" {
		return best, strings.TrimPrefix(rel, bestRoot+"/"), nil
	}
	return loadfile.Volume{}, "", loadfile.ErrUnsafeReference
}

func packageMappingSHA256(mapping loadfile.Mapping) (string, error) {
	encoded, err := canonical.Marshal(mapping)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func packageDiagnostics(values []loadfile.Diagnostic) []PackageDiagnostic {
	if len(values) > maxPackageDiagnosticSummary {
		values = values[:maxPackageDiagnosticSummary]
	}
	result := make([]PackageDiagnostic, len(values))
	for i, value := range values {
		result[i] = PackageDiagnostic{Code: value.Code, Severity: value.Severity, LoadFile: value.LoadFile, RowID: value.RowID, RowOrdinal: value.RowOrdinal, Column: value.Column, Detail: value.Detail}
	}
	return result
}

func packagePreflightFromRecord(record store.PackagePreflightRecord) (PackagePreflight, error) {
	var result PackagePreflight
	if err := json.Unmarshal(record.CanonicalJSON, &result); err != nil {
		return PackagePreflight{}, fmt.Errorf("decode package preflight summary: %w", err)
	}
	result.PreflightID, result.SourceKind, result.SourceRef = record.PreflightID, record.SourceKind, record.SourceRef
	result.ProfileSHA256, result.MappingSHA256, result.ManifestSHA256 = record.ProfileSHA256, record.MappingSHA256, record.ManifestSHA256
	result.Blocking, result.CreatedAt, result.ExpiresAt = record.Blocking, record.CreatedAt, record.ExpiresAt
	return result, nil
}

func writePackageError(w http.ResponseWriter, err error) {
	mapped, ok := errors.AsType[*Error](FromStoreError(err))
	if !ok {
		mapped = NewError(http.StatusInternalServerError, "internal", err.Error())
	}
	writeError(w, mapped)
}
