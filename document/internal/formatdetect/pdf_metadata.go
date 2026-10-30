package formatdetect

import (
	"errors"
	"fmt"
	"math"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDFFacts records declared PDF properties, without validating signatures.
// Nil observations could not be established; zero and false are observations.
type PDFFacts struct {
	Encrypted               *bool
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
		facts.Permissions = pdfPermissionNames(ctx.E.R, ctx.E.P)
	}
	embedded := map[int]bool{}
	var javascript, launch bool
	_, err := pdfWalkDicts(ctx, func(dictionary types.Dict) (bool, error) {
		action, err := pdfDictName(ctx, dictionary, "S")
		if err != nil {
			return false, err
		}
		javascript = javascript || (action == "JavaScript" || action == "Rendition") && dictionary["JS"] != nil
		launch = launch || action == "Launch" && (dictionary["F"] != nil || dictionary["Win"] != nil || dictionary["Mac"] != nil || dictionary["Unix"] != nil)
		streams, err := pdfEmbeddedFileStreams(ctx, dictionary)
		for _, object := range streams {
			if ref, ok := object.(types.IndirectRef); ok {
				embedded[ref.ObjectNumber.Value()] = true
			}
		}
		return false, err
	})
	if err != nil {
		metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "Evidence"})
	} else {
		facts.EmbeddedFileCount, facts.HasJavaScript, facts.HasLaunchActions = new(int64(len(embedded))), new(javascript), new(launch)
	}
	count, endpoint, malformed, err := pdfSignatureFacts(ctx)
	if err != nil {
		metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "SignatureFields"})
	} else {
		facts.SignatureFieldCount = new(count)
		if malformed {
			metadata.Issues = append(metadata.Issues, PDFMetadataIssue{SourceField: "ByteRange"})
		}
		if endpoint == size || !malformed && endpoint >= 0 {
			facts.LastSignatureCoversFile = new(endpoint == size)
		}
	}
	metadata.Facts = facts
}

func pdfSignatureFacts(ctx *model.Context) (count, endpoint int64, malformed bool, err error) {
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
	var walk func(types.Object, string, types.Object, int) error
	walk = func(object types.Object, inherited string, value types.Object, depth int) error {
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
			return nil
		}
		ft, err := pdfDictName(ctx, field, "FT")
		if err != nil {
			return fmt.Errorf("inspect PDF evidence: %w", err)
		}
		if ft == "" {
			ft = inherited
		}
		if ownValue, exists := field.Find("V"); exists {
			value = ownValue
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
			if !dict.HasEntry("T") && !dict.HasEntry("FT") && !dict.HasEntry("V") && !dict.HasEntry("Kids") {
				continue
			}
			hasFields = true
			if err := walk(kid, ft, value, depth+1); err != nil {
				return fmt.Errorf("inspect PDF evidence: %w", err)
			}
		}
		if ft != "Sig" || hasFields {
			return nil
		}
		count++
		signedEnd, err := pdfSignatureEndpoint(ctx, value)
		if err != nil {
			malformed = true
		} else {
			endpoint = max(endpoint, signedEnd)
		}
		return nil
	}
	for _, field := range fields {
		if err := walk(field, "", nil, 0); err != nil {
			return 0, -1, false, fmt.Errorf("read signature fields: %w", err)
		}
	}
	return count, endpoint, malformed, nil
}

func pdfSignatureEndpoint(ctx *model.Context, object types.Object) (int64, error) {
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
	if numbers[0] != 0 || numbers[1] > numbers[2] || numbers[3] > math.MaxInt64-numbers[2] {
		return -1, errors.New("signature ranges overlap or overflow")
	}
	return numbers[2] + numbers[3], nil
}

func pdfPermissionNames(revision, bits int) []string {
	var names []string
	for _, permission := range []struct {
		flag model.PermissionFlags
		name string
	}{
		{model.PermissionPrintRev2, "Print"}, {model.PermissionModify, "Modify"}, {model.PermissionExtract, "Copy text and graphics"},
		{model.PermissionModAnnFillForm, "Annotate and fill forms"}, {model.PermissionFillRev3, "Fill forms"}, {model.PermissionExtractRev3, "Extract for accessibility"},
		{model.PermissionAssembleRev3, "Assemble"}, {model.PermissionPrintRev3, "High-quality print flag"},
	} {
		if (revision >= 3 || permission.flag <= model.PermissionModAnnFillForm) && model.PermissionFlags(bits)&permission.flag != 0 {
			names = append(names, permission.name)
		}
	}
	if len(names) == 0 {
		return []string{"None"}
	}
	return names
}
