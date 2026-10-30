package formatdetect

import (
	"errors"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDFFacts records declared PDF properties, without validating signatures.
// Nil observations could not be established; zero and false are observations.
type PDFFacts struct {
	Encrypted               *bool
	PermissionBits          *int64
	Permissions             []string
	SignatureFieldCount     *int64
	LastSignatureCoversFile *bool
	EmbeddedFileCount       *int64
	HasJavaScript           *bool
	HasLaunchActions        *bool
}

func readPDFFacts(ctx *model.Context, size int64, metadata *PDFMetadata) {
	facts := PDFFacts{Encrypted: new(ctx.Encrypt != nil)}
	if ctx.E != nil {
		facts.PermissionBits = new(int64(ctx.E.P))
		facts.Permissions = pdfcpu.PermissionsList(ctx.E.P)
	} else {
		facts.Permissions = pdfcpu.PermissionsList(0)
	}
	var embedded int64
	var javascript, launch bool
	visited := map[int]bool{}
	var walk func(types.Object, int) error
	walk = func(object types.Object, depth int) error {
		if depth > maxPDFPageTreeDepth {
			return errors.New("PDF object depth exceeded")
		}
		if ref, ok := object.(types.IndirectRef); ok {
			number := ref.ObjectNumber.Value()
			if visited[number] {
				return nil
			}
			visited[number] = true
			resolved, err := ctx.Dereference(ref)
			if err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
			return walk(resolved, depth+1)
		}
		if stream, ok := pdfStreamDictionary(object); ok {
			object = stream.Dict
		}
		switch value := object.(type) {
		case types.Dict:
			action, err := pdfDictName(ctx, value, "S")
			if err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
			javascript = javascript || action == "JavaScript"
			launch = launch || action == "Launch"
			carries, err := pdfCarriesEmbeddedFile(ctx, value)
			if err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
			if carries {
				embedded++
			}
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return fmt.Errorf("inspect PDF evidence: %w", err)
				}
			}
		case types.Array:
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return fmt.Errorf("inspect PDF evidence: %w", err)
				}
			}
		}
		return nil
	}
	names, namesErr := ctx.DereferenceDict(ctx.RootDict["Names"])
	if namesErr == nil && names != nil {
		tree, err := ctx.DereferenceDict(names["JavaScript"])
		namesErr = err
		javascript = tree != nil
	}
	if err := walk(*ctx.Root, 0); err != nil || namesErr != nil {
		metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "Evidence"})
	} else {
		facts.EmbeddedFileCount, facts.HasJavaScript, facts.HasLaunchActions = new(embedded), new(javascript), new(launch)
	}
	count, endpoint, malformed, err := pdfSignatureFacts(ctx, size)
	if err != nil {
		metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "SignatureFields"})
	} else {
		facts.SignatureFieldCount = new(count)
		if malformed {
			metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "ByteRange"})
		} else if endpoint >= 0 {
			facts.LastSignatureCoversFile = new(endpoint == size)
		}
	}
	metadata.Facts = facts
}

func pdfSignatureFacts(ctx *model.Context, size int64) (count, endpoint int64, malformed bool, err error) {
	endpoint = -1
	form, err := ctx.DereferenceDict(ctx.RootDict["AcroForm"])
	if err != nil {
		return 0, -1, false, fmt.Errorf("read signature form: %w", err)
	}
	if form == nil {
		return 0, -1, false, nil
	}
	fields, err := ctx.DereferenceArray(form["Fields"])
	if err != nil {
		return 0, -1, false, fmt.Errorf("read signature fields: %w", err)
	}
	visited := map[int]bool{}
	var walk func(types.Object, string, int) error
	walk = func(object types.Object, inherited string, depth int) error {
		if depth > maxPDFPageTreeDepth {
			return errors.New("PDF field depth exceeded")
		}
		if ref, ok := object.(types.IndirectRef); ok {
			if visited[ref.ObjectNumber.Value()] {
				return nil
			}
			visited[ref.ObjectNumber.Value()] = true
		}
		field, err := ctx.DereferenceDict(object)
		if err != nil {
			return fmt.Errorf("inspect PDF evidence: %w", err)
		}
		if field == nil {
			return errors.New("PDF field missing")
		}
		ft, err := pdfDictName(ctx, field, "FT")
		if err != nil {
			return fmt.Errorf("inspect PDF evidence: %w", err)
		}
		if ft == "" {
			ft = inherited
		}
		kids, err := ctx.DereferenceArray(field["Kids"])
		if err != nil {
			return fmt.Errorf("inspect PDF evidence: %w", err)
		}
		hasFields := false
		for _, kid := range kids {
			dict, err := ctx.DereferenceDict(kid)
			if err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
			subtype, err := pdfDictName(ctx, dict, "Subtype")
			if err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
			if subtype == "Widget" && !dict.HasEntry("T") && !dict.HasEntry("FT") {
				continue
			}
			hasFields = true
			if err := walk(kid, ft, depth+1); err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
		}
		if ft != "Sig" || hasFields {
			return nil
		}
		count++
		signedEnd, err := pdfSignatureEndpoint(ctx, field["V"], size)
		if err != nil {
			malformed = true
		} else {
			endpoint = max(endpoint, signedEnd)
		}
		return nil
	}
	for _, field := range fields {
		if err := walk(field, "", 0); err != nil {
			return 0, -1, false, fmt.Errorf("read signature fields: %w", err)
		}
	}
	return count, endpoint, malformed, nil
}

func pdfSignatureEndpoint(ctx *model.Context, object types.Object, size int64) (int64, error) {
	value, err := ctx.DereferenceDict(object)
	if err != nil {
		return -1, fmt.Errorf("read signature value: %w", err)
	}
	if value == nil {
		return -1, nil
	}
	ranges, err := ctx.DereferenceArray(value["ByteRange"])
	if err != nil {
		return -1, fmt.Errorf("read signature range: %w", err)
	}
	if len(ranges) != 4 {
		return -1, errors.New("signature range needs four integers")
	}
	var numbers [4]int64
	for i, object := range ranges {
		resolved, err := ctx.Dereference(object)
		if err != nil {
			return -1, fmt.Errorf("read signature range integer: %w", err)
		}
		number, ok := resolved.(types.Integer)
		if !ok || number < 0 {
			return -1, errors.New("signature range needs nonnegative integers")
		}
		numbers[i] = int64(number)
	}
	if numbers[0] != 0 || numbers[1] > numbers[2] || numbers[2] > size || numbers[3] > size-numbers[2] {
		return -1, errors.New("signature range outside original")
	}
	return numbers[2] + numbers[3], nil
}
