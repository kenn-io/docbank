package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	documentcoverage "go.kenn.io/docbank/document/coverage"
	internalformatcoverage "go.kenn.io/docbank/internal/formatcoverage"
)

func freezeFormatCoverage(d Deps) Deps {
	provider := d.FormatCoverage
	if provider == nil {
		provider = func(context.Context) (document.FormatCoverageV1, error) {
			return internalformatcoverage.Compute(nil)
		}
	}
	snapshot, snapshotErr := provider(context.Background())
	if snapshotErr == nil {
		snapshotErr = document.ValidateFormatCoverageV1(snapshot)
	}
	snapshot = document.CloneFormatCoverageV1(snapshot)
	d.FormatCoverage = func(ctx context.Context) (document.FormatCoverageV1, error) {
		if err := ctx.Err(); err != nil {
			return document.FormatCoverageV1{}, err
		}
		return document.CloneFormatCoverageV1(snapshot), snapshotErr
	}
	return d
}

func (d Deps) formatCoverage(ctx context.Context) (document.FormatCoverageV1, error) {
	if d.FormatCoverage == nil {
		return document.FormatCoverageV1{}, errors.New("format coverage dependency is not configured")
	}
	return d.FormatCoverage(ctx)
}

func validateFormatQuery(family, format, extension string) error {
	if format != "" && extension != "" {
		return errFormatQuery
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "family", value: family, limit: 64},
		{name: "format", value: format, limit: 64},
		{name: "extension", value: extension, limit: 16},
	} {
		if len(field.value) > field.limit {
			return fmt.Errorf("%s must not exceed %d bytes", field.name, field.limit)
		}
	}
	return nil
}

func registerFormatRoutes(api huma.API, d Deps) {
	type formatCoverageOutput struct{ Body FormatCoverageResponse }
	huma.Register(api, huma.Operation{
		OperationID: "readFormatCapabilities", Method: http.MethodGet,
		Path: "/api/v1/formats/capabilities", Summary: "Read per-format capability coverage",
	}, func(ctx context.Context, in *struct {
		Family    string `query:"family" maxLength:"64"`
		Format    string `query:"format" maxLength:"64"`
		Extension string `query:"extension" maxLength:"16"`
	}) (*formatCoverageOutput, error) {
		if err := validateFormatQuery(in.Family, in.Format, in.Extension); err != nil {
			if errors.Is(err, errFormatQuery) {
				return nil, fromFormatQueryError(err)
			}
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_format_query", err.Error())
		}
		coverage, err := d.formatCoverage(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}

		var lookup *document.FormatLookupV1
		selector := in.Format
		if selector == "" {
			selector = in.Extension
		}
		if selector != "" {
			resolved := documentcoverage.Lookup(coverage, selector)
			lookup = &resolved
		}

		filtered := make([]document.FormatCapabilityV1, 0, len(coverage.Formats))
		for _, format := range coverage.Formats {
			if in.Family != "" && format.QueryFamily != in.Family {
				continue
			}
			if lookup != nil && (lookup.Format == nil || lookup.Format.ID != format.ID) {
				continue
			}
			filtered = append(filtered, format)
		}
		coverage.Formats = filtered
		return &formatCoverageOutput{Body: FormatCoverageResponse{
			FormatCoverageV1: coverage, Lookup: lookup,
		}}, nil
	})
}
