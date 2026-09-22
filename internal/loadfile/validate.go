package loadfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

type ValidateInput struct {
	Records   []Record
	Images    []ImageRef
	Volumes   []Volume
	Resolver  *Resolver
	PageCount func(file io.ReadSeeker) (int, error)
}

// Validate hashes each referenced file while validating it. PDF page counts use a
// private snapshot of those same bytes; this does not snapshot the whole package.
func Validate(ctx context.Context, in ValidateInput) ([]FileRef, []Diagnostic, error) {
	if in.Resolver == nil {
		return nil, nil, ErrUnsafeReference
	}
	volumes := make(map[string]Volume, len(in.Volumes))
	for _, volume := range in.Volumes {
		volumes[volume.Name] = volume
	}
	declaredPages := make(map[string]int)
	for _, image := range in.Images {
		if image.DocumentBreak && image.DeclaredPageCount > 0 && declaredPages[image.ImageKey] == 0 {
			declaredPages[image.ImageKey] = image.DeclaredPageCount
		}
	}
	files := make([]FileRef, 0, len(in.Images))
	diagnostics := make([]Diagnostic, 0)
	addDiagnostic := func(diagnostic Diagnostic) error {
		return appendDiagnosticBounded(&diagnostics, diagnostic)
	}
	records := make(map[string]Record, len(in.Records))
	for _, record := range in.Records {
		if err := ctx.Err(); err != nil {
			return files, diagnostics, err
		}
		if record.DocID == "" {
			if err := addDiagnostic(packageDiagnostic("missing_document_id", record, "record has no mapped document id")); err != nil {
				return files, diagnostics, err
			}
		} else if previous, exists := records[record.DocID]; exists {
			if err := addDiagnostic(packageDiagnostic("duplicate_document_id", record, "document id is also used by row "+previous.RowID)); err != nil {
				return files, diagnostics, err
			}
		} else {
			records[record.DocID] = record
		}
	}
	for _, record := range in.Records {
		if err := ctx.Err(); err != nil {
			return files, diagnostics, err
		}
		if parent := record.Family.ParentDocID; parent != "" {
			if _, exists := records[parent]; !exists {
				if err := addDiagnostic(packageDiagnostic("family_edge_unresolved", record, "parent document id is absent")); err != nil {
					return files, diagnostics, err
				}
			}
		}
		for _, child := range record.Family.AttachmentDocIDs {
			if _, exists := records[child]; !exists {
				if err := addDiagnostic(packageDiagnostic("family_edge_unresolved", record, "attachment document id is absent")); err != nil {
					return files, diagnostics, err
				}
			}
		}
		for fileIndex := range record.Files {
			fileRef := &record.Files[fileIndex]
			if fileRef.Status != "" && fileRef.Status != "available" {
				continue
			}
			volume, exists := volumes[fileRef.Volume]
			var checkPDF func(io.ReadSeeker) error
			if in.PageCount != nil && (fileRef.Role == "native" || fileRef.Role == "produced_pdf") && strings.EqualFold(filepath.Ext(fileRef.RelPath), ".pdf") {
				checkPDF = func(file io.ReadSeeker) error {
					actual, countErr := in.PageCount(file)
					if countErr != nil {
						return addDiagnostic(packageDiagnostic("page_count_unavailable", record, countErr.Error()))
					}
					if declared := declaredPages[record.DocID]; declared > 0 && actual != declared {
						return addDiagnostic(packageDiagnostic("page_count_mismatch", record, fmt.Sprintf("declared %d pages; source has %d", declared, actual)))
					}
					fileRef.VerifiedPageCount = actual
					return nil
				}
			}
			var err error
			if exists {
				*fileRef, err = validatePackageFile(ctx, in.Resolver, volume, *fileRef, checkPDF)
			} else {
				err = ErrUnsafeReference
			}
			if err != nil {
				if !errors.Is(err, ErrUnsafeReference) {
					return files, diagnostics, err
				}
				fileRef.Status = "missing"
				if err := addDiagnostic(packageDiagnostic("file_missing", record, "declared file is absent or unsafe")); err != nil {
					return files, diagnostics, err
				}
			}
			files = append(files, *fileRef)
		}
	}
	for index, image := range in.Images {
		if err := ctx.Err(); err != nil {
			return files, diagnostics, err
		}
		if index == 0 && !image.DocumentBreak {
			if err := addDiagnostic(Diagnostic{Code: "image_boundary_missing", Severity: diagnosticSeverityBlocking, RowID: image.ImageKey, RowOrdinal: 1, Detail: "first page-map image must start a document"}); err != nil {
				return files, diagnostics, err
			}
		}
		if image.DocumentBreak {
			record, exists := records[image.ImageKey]
			if !exists {
				if err := addDiagnostic(Diagnostic{Code: "image_identity_unresolved", Severity: diagnosticSeverityBlocking, RowID: image.ImageKey, RowOrdinal: index + 1, Detail: "page-map document boundary has no metadata record"}); err != nil {
					return files, diagnostics, err
				}
			} else if image.Boundary == "child" && record.Family.ParentDocID == "" {
				if err := addDiagnostic(Diagnostic{Code: "family_edge_unresolved", Severity: diagnosticSeverityBlocking, RowID: image.ImageKey, RowOrdinal: index + 1, Detail: "child page-map boundary has no metadata parent"}); err != nil {
					return files, diagnostics, err
				}
			}
		}
		volume, exists := volumes[image.Volume]
		ref := FileRef{Role: "page_image", Volume: image.Volume, RelPath: image.RelPath, Declared: image.RelPath}
		var err error
		if exists {
			ref, err = validatePackageFile(ctx, in.Resolver, volume, ref, nil)
		} else {
			err = ErrUnsafeReference
		}
		if err != nil {
			if !errors.Is(err, ErrUnsafeReference) {
				return files, diagnostics, err
			}
			ref.Status = "missing"
			if err := addDiagnostic(Diagnostic{Code: "image_missing", Severity: diagnosticSeverityBlocking, RowID: image.ImageKey, RowOrdinal: index + 1, Detail: "declared image is absent or unsafe"}); err != nil {
				return files, diagnostics, err
			}
		}
		files = append(files, ref)
	}
	for _, diagnostic := range familyCycleDiagnostics(in.Records) {
		if err := addDiagnostic(diagnostic); err != nil {
			return files, diagnostics, err
		}
	}
	return files, diagnostics, nil
}

func packageDiagnostic(code string, record Record, detail string) Diagnostic {
	return Diagnostic{Code: code, Severity: diagnosticSeverityBlocking, LoadFile: record.LoadFile, RowID: record.RowID, RowOrdinal: record.RowOrdinal, Detail: detail}
}

func familyCycleDiagnostics(records []Record) []Diagnostic {
	parents := make(map[string][]string, len(records))
	byID := make(map[string]Record, len(records))
	for _, record := range records {
		if record.DocID != "" {
			byID[record.DocID] = record
			if record.Family.ParentDocID != "" {
				parents[record.DocID] = append(parents[record.DocID], record.Family.ParentDocID)
			}
			for _, child := range record.Family.AttachmentDocIDs {
				parents[child] = append(parents[child], record.DocID)
			}
		}
	}
	for id := range parents {
		slices.Sort(parents[id])
	}
	state := make(map[string]uint8, len(parents))
	stack := make([]string, 0, len(parents))
	stackIndex := make(map[string]int, len(parents))
	cycleMembers := make(map[string]bool)
	var visit func(string)
	visit = func(id string) {
		state[id] = 1
		stackIndex[id] = len(stack)
		stack = append(stack, id)
		for _, next := range parents[id] {
			switch state[next] {
			case 0:
				visit(next)
			case 1:
				for _, member := range stack[stackIndex[next]:] {
					cycleMembers[member] = true
				}
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, id)
		state[id] = 2
	}
	ids := make([]string, 0, len(parents))
	for id := range parents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if state[id] == 0 {
			visit(id)
		}
	}
	cycleIDs := make([]string, 0, len(cycleMembers))
	for id := range cycleMembers {
		cycleIDs = append(cycleIDs, id)
	}
	slices.Sort(cycleIDs)
	result := make([]Diagnostic, 0, len(cycleIDs))
	for _, id := range cycleIDs {
		result = append(result, packageDiagnostic("family_cycle", byID[id], "family relationship contains a cycle"))
	}
	return result
}

func Blocking(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == diagnosticSeverityBlocking {
			return true
		}
	}
	return false
}
