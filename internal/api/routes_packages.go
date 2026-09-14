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

func registerPackageRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate, roots *packageRootRegistry) {
	registerPackageOpenAPI(api)
	mux.HandleFunc("POST /api/v1/packages/preflights", func(w http.ResponseWriter, r *http.Request) {
		handlePackagePreflight(w, r, d, g, roots)
	})
	mux.HandleFunc("POST /api/v1/packages/containers/{container_id}/preflight", func(w http.ResponseWriter, r *http.Request) {
		handlePackagePreflight(w, r, d, g, roots)
	})
	mux.HandleFunc("GET /api/v1/packages/preflights/{preflight_id}", func(w http.ResponseWriter, r *http.Request) {
		owner, ok := workspaceSnapshotOwner(r.Context())
		if !ok {
			writeError(w, NewError(http.StatusInternalServerError, "internal", "authenticated request owner is unavailable"))
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
	for _, operation := range []*huma.Operation{
		{OperationID: "createPackagePreflight", Method: http.MethodPost, Path: "/api/v1/packages/preflights", Summary: "Preview a load-file package", MaxBodyBytes: maxPackageRequestBytes},
		{OperationID: "createPackageContainerPreflight", Method: http.MethodPost, Path: "/api/v1/packages/containers/{container_id}/preflight", Summary: "Preview a sealed package container", MaxBodyBytes: maxPackageRequestBytes},
		{OperationID: "readPackagePreflight", Method: http.MethodGet, Path: "/api/v1/packages/preflights/{preflight_id}", Summary: "Read an expiring package preview"},
		{OperationID: "readPackagePreflightDiagnostics", Method: http.MethodGet, Path: "/api/v1/packages/preflights/{preflight_id}/diagnostics", Summary: "Read one bounded page of package diagnostics"},
	} {
		api.OpenAPI().AddOperation(operation)
	}
}

func handlePackageDiagnostics(w http.ResponseWriter, r *http.Request, d Deps) {
	owner, ok := workspaceSnapshotOwner(r.Context())
	if !ok {
		writeError(w, NewError(http.StatusInternalServerError, "internal", "authenticated request owner is unavailable"))
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

func handlePackagePreflight(w http.ResponseWriter, r *http.Request, d Deps, g *gate, roots *packageRootRegistry) {
	var request PackagePreflightRequest
	if err := readPackageJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	owner, ok := workspaceSnapshotOwner(r.Context())
	if !ok {
		writeError(w, NewError(http.StatusInternalServerError, "internal", "authenticated request owner is unavailable"))
		return
	}
	if request.SourceKind != "root" {
		writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "sealed package containers are admitted by the package import slice"))
		return
	}
	var result PackagePreflight
	err := g.MutateContext(r.Context(), func() error {
		var buildErr error
		result, buildErr = buildPackagePreflight(r.Context(), d, owner, request, roots)
		return buildErr
	})
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

func buildPackagePreflight(ctx context.Context, d Deps, owner string, request PackagePreflightRequest, roots *packageRootRegistry) (PackagePreflight, error) {
	profile, err := loadfile.ReadProfile(request.Profile)
	if err != nil {
		return PackagePreflight{}, err
	}
	profile.Encoding = request.Encoding
	if _, err := loadfile.Decoder(profile.Encoding); err != nil {
		return PackagePreflight{}, err
	}
	plainResolver, err := loadfile.NewResolver(request.SourceRef, nil)
	if err != nil {
		return PackagePreflight{}, err
	}
	defer func() { _ = plainResolver.Close() }()
	datName, optName, discoveredVolumes, err := plainResolver.DiscoverPackageFiles()
	if err != nil {
		return PackagePreflight{}, err
	}
	datPath := filepath.Join(plainResolver.Root, filepath.FromSlash(datName))
	optPath := ""
	if optName != "" {
		optPath = filepath.Join(plainResolver.Root, filepath.FromSlash(optName))
	}
	datVolume, datRel, err := packagePathReference(plainResolver.Root, datPath, discoveredVolumes, nil)
	if err != nil {
		return PackagePreflight{}, err
	}
	dat, err := plainResolver.Open(datVolume, datRel)
	if err != nil {
		return PackagePreflight{}, err
	}
	records := make([]loadfile.Record, 0, 100)
	memoryBudget := packageMemoryBudget{maximum: maxPackageNormalizedMemory}
	diagnostics, parseErr := loadfile.ScanDAT(dat, profile, func(record loadfile.Record) error {
		if len(records) == maxPackageRecords {
			return loadfile.ErrLoadfileLimit
		}
		if err := memoryBudget.add(packageRecordMemory(record)); err != nil {
			return err
		}
		records = append(records, record)
		return nil
	})
	closeErr := dat.Close()
	if parseErr != nil || closeErr != nil {
		return PackagePreflight{}, errors.Join(parseErr, closeErr)
	}
	for i := range records {
		records[i].LoadFile = filepath.Base(datPath)
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
	if len(mapping.Columns) == 0 {
		for i := range records {
			hydratePackageRecord(&records[i])
		}
	} else {
		mappedDiagnostics, applyErr := loadfile.ApplyMapping(records, mapping, profile)
		if applyErr != nil {
			return PackagePreflight{}, applyErr
		}
		diagnostics = append(diagnostics, mappedDiagnostics...)
	}
	volumes := logicalPackageVolumes(discoveredVolumes, mapping)
	loadfile.NormalizeFileReferences(records, volumes)
	if err := memoryBudget.reset(packageRecordsMemory(records)); err != nil {
		return PackagePreflight{}, err
	}
	resolver, err := loadfile.NewResolver(request.SourceRef, mapping.VolumeRoots)
	if err != nil {
		return PackagePreflight{}, err
	}
	retainResolver := false
	defer func() {
		if !retainResolver {
			_ = resolver.Close()
		}
	}()
	images := []loadfile.ImageRef{}
	if optPath != "" {
		optVolume, optRel, pathErr := packagePathReference(plainResolver.Root, optPath, volumes, mapping.VolumeRoots)
		if pathErr != nil {
			return PackagePreflight{}, pathErr
		}
		opt, openErr := resolver.Open(optVolume, optRel)
		if openErr != nil {
			return PackagePreflight{}, openErr
		}
		optProfile, profileErr := loadfile.ReadProfile("opt-standard-v1")
		if profileErr != nil {
			_ = opt.Close()
			return PackagePreflight{}, profileErr
		}
		optProfile.Encoding = request.Encoding
		var optDiagnostics []loadfile.Diagnostic
		optDiagnostics, parseErr = loadfile.ScanOPT(opt, optProfile, func(image loadfile.ImageRef) error {
			if len(images) == maxPackagePages {
				return loadfile.ErrLoadfileLimit
			}
			if err := memoryBudget.add(packageImageMemory(image)); err != nil {
				return err
			}
			images = append(images, image)
			return nil
		})
		closeErr = opt.Close()
		diagnostics = append(diagnostics, optDiagnostics...)
		if parseErr != nil || closeErr != nil {
			return PackagePreflight{}, errors.Join(parseErr, closeErr)
		}
	}
	validated, err := loadfile.Validate(ctx, loadfile.ValidateInput{Profile: profile, Mapping: mapping, Records: records, Images: images, Volumes: volumes, Resolver: resolver, PageCount: func(file *os.File) (int, error) {
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
	datVolume, datRel, err = packagePathReference(plainResolver.Root, datPath, volumes, mapping.VolumeRoots)
	if err != nil {
		return PackagePreflight{}, err
	}
	loadFileCount := 1
	if optPath != "" {
		loadFileCount++
	}
	if err := memoryBudget.add(packageManifestFilesMemory(records, images, loadFileCount)); err != nil {
		return PackagePreflight{}, err
	}
	files, err := hashPackageFiles(resolver, volumes, records, images, []packageLoadFile{{volume: datVolume, relPath: datRel}})
	if err != nil {
		return PackagePreflight{}, err
	}
	if optPath != "" {
		optVolume, optRel, pathErr := packagePathReference(plainResolver.Root, optPath, volumes, mapping.VolumeRoots)
		if pathErr != nil {
			return PackagePreflight{}, pathErr
		}
		extra, hashErr := hashPackageFile(resolver, optVolume, optRel, "raw_load_file")
		if hashErr != nil {
			return PackagePreflight{}, hashErr
		}
		files = append(files, extra)
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
	stored, err := persistPackagePreflight(ctx, d, owner, manifest, diagnostics, out)
	if err != nil {
		return PackagePreflight{}, err
	}
	if err := roots.register(owner, stored.PreflightID, stored.SourceRef, stored.ExpiresAt, resolver); err != nil {
		return PackagePreflight{}, err
	}
	retainResolver = true
	return stored, nil
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

func persistPackagePreflight(ctx context.Context, d Deps, owner string, manifest loadfile.Manifest, diagnostics []loadfile.Diagnostic, out PackagePreflight) (PackagePreflight, error) {
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
	record := store.PackagePreflightRecord{PreflightID: out.PreflightID, Owner: owner, SourceKind: out.SourceKind, SourceRef: out.SourceRef, ProfileSHA256: out.ProfileSHA256, MappingSHA256: out.MappingSHA256, ManifestSHA256: out.ManifestSHA256, CanonicalJSON: summaryBytes, DiagnosticsJSON: diagnosticSummary, Blocking: out.Blocking}
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

func logicalPackageVolumes(discovered []loadfile.Volume, mapping loadfile.Mapping) []loadfile.Volume {
	if len(mapping.VolumeRoots) == 0 {
		return discovered
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
	return volumes
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

type packageLoadFile struct {
	volume  loadfile.Volume
	relPath string
}

func hashPackageFiles(resolver *loadfile.Resolver, volumes []loadfile.Volume, records []loadfile.Record, images []loadfile.ImageRef, loadFiles []packageLoadFile) ([]loadfile.FileRef, error) {
	byName := make(map[string]loadfile.Volume, len(volumes))
	for _, volume := range volumes {
		byName[volume.Name] = volume
	}
	files := make([]loadfile.FileRef, 0, len(images)+len(loadFiles))
	for recordIndex := range records {
		for fileIndex := range records[recordIndex].Files {
			ref := &records[recordIndex].Files[fileIndex]
			volume, ok := byName[ref.Volume]
			if !ok || ref.Status != "available" {
				continue
			}
			hashed, err := hashPackageFile(resolver, volume, ref.RelPath, ref.Role)
			if err != nil {
				if errors.Is(err, loadfile.ErrUnsafeReference) {
					ref.Status = "missing"
					files = append(files, *ref)
					continue
				}
				return nil, err
			}
			hashed.Declared = ref.Declared
			*ref = hashed
			files = append(files, hashed)
		}
	}
	for _, image := range images {
		volume, ok := byName[image.Volume]
		if !ok {
			continue
		}
		hashed, err := hashPackageFile(resolver, volume, image.RelPath, "page_image")
		if err != nil {
			if errors.Is(err, loadfile.ErrUnsafeReference) {
				files = append(files, loadfile.FileRef{Role: "page_image", Volume: volume.Name, RelPath: image.RelPath, Declared: image.RelPath, Status: "missing"})
				continue
			}
			return nil, err
		}
		files = append(files, hashed)
	}
	for _, loadFile := range loadFiles {
		hashed, err := hashPackageFile(resolver, loadFile.volume, loadFile.relPath, "raw_load_file")
		if err != nil {
			return nil, err
		}
		files = append(files, hashed)
	}
	return files, nil
}

func hashPackageFile(resolver *loadfile.Resolver, volume loadfile.Volume, relPath, role string) (loadfile.FileRef, error) {
	file, err := resolver.Open(volume, relPath)
	if err != nil {
		return loadfile.FileRef{}, err
	}
	digest := sha256.New()
	size, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return loadfile.FileRef{}, errors.Join(copyErr, closeErr)
	}
	return loadfile.FileRef{Role: role, Volume: volume.Name, RelPath: relPath, Declared: relPath, SHA256: hex.EncodeToString(digest.Sum(nil)), Status: "available", Size: size}, nil
}

func hydratePackageRecord(record *loadfile.Record) {
	for _, field := range record.Fields {
		switch strings.ToUpper(field.Column) {
		case "DOCID", "BEGDOC":
			record.DocID = field.Raw
		case "PARENT", "PARENTID":
			record.Family.ParentDocID = field.Raw
		case "NATIVE", "NATIVEFILE", "NATIVEPATH":
			appendPackageFile(record, "native", field.Raw)
		case "TEXT", "TEXTPATH":
			appendPackageFile(record, "supplied_text", field.Raw)
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("package-row/v1\x00%s\x00%d\x00%s", record.LoadFile, record.RowOrdinal, record.DocID)))
	record.RowID = hex.EncodeToString(sum[:])
}

func appendPackageFile(record *loadfile.Record, role, path string) {
	if path == "" {
		return
	}
	clean := strings.ReplaceAll(path, `\`, "/")
	record.Files = append(record.Files, loadfile.FileRef{Role: role, RelPath: clean, Declared: path, Status: "available"})
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
