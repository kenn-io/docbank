package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
	"golang.org/x/text/cases"

	"go.kenn.io/docbank/internal/query"
)

func compilePhotoAssetPredicate(predicate string, args ...any) compiledQueryFragment {
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files pf JOIN photo_assets pa ON pa.asset_id=pf.asset_id WHERE pf.node_id=n.id AND ` + predicate + `)`, args: args}
}

func (c queryCompiler) compilePhotoVersionPredicate(predicate func(alias string) string, args ...any) compiledQueryFragment {
	if c.photoDisplayMetadata {
		return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member
 JOIN photo_assets asset ON asset.asset_id=member.asset_id
 JOIN photo_files display ON display.file_id=asset.display_file_id
 JOIN nodes display_node ON display_node.id=display.node_id
 JOIN content_versions v ON v.version_id=display_node.current_version_id
 WHERE member.node_id=n.id AND ` + predicate("v") + `)`, args: args}
	}
	return compiledQueryFragment{sql: predicate("cv"), args: args}
}

func (c queryCompiler) compilePhotoDecisionPredicate(predicate string, args ...any) compiledQueryFragment {
	if c.photoDisplayMetadata {
		return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member
 JOIN photo_assets asset ON asset.asset_id=member.asset_id
 JOIN photo_files pf ON pf.file_id=asset.display_file_id
 WHERE member.node_id=n.id AND ` + predicate + `)`, args: args}
	}
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files pf WHERE pf.node_id=n.id AND pf.role<>'sidecar' AND ` + predicate + `)`, args: args}
}

func isPhotoScalarField(field string) bool {
	return query.IsQualityField(field) || slices.Contains([]string{"rating", "rating_min", "rating_max", "flag", "label", "kind", "camera", "lens", "iso", "iso_min", "iso_max", "capture_after", "capture_before", "gps", "asset", "set"}, field)
}

func (c queryCompiler) compilePhotoMetadataPredicate(predicate string, args ...any) compiledQueryFragment {
	return c.compilePhotoVersionPredicate(func(alias string) string {
		return `EXISTS (SELECT 1 FROM source_metadata_heads h JOIN photo_technical_metadata p ON p.generation_id=h.generation_id WHERE h.source_sha256=` + alias + `.blob_hash AND ` + predicate + `)`
	}, args...)
}

func (c queryCompiler) compilePhotoScalarPredicate(field, value string) (compiledQueryFragment, error) {
	if query.IsQualityField(field) {
		if field == "unevaluated" {
			if value != "true" {
				return compiledQueryFragment{}, errors.New("unevaluated must be true")
			}
		} else {
			normalized, err := query.NormalizeQualityOperand(value)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			value = normalized
		}
		return c.compilePhotoQualityPredicate(field, value)
	}
	switch field {
	case "rating", "rating_min", "rating_max":
		n, err := query.ParseSizeOperand(value)
		if err != nil || n > 5 {
			return compiledQueryFragment{}, errors.New("rating must be 0 through 5")
		}
		operator := "="
		if field == "rating_min" {
			operator = ">="
		}
		if field == "rating_max" {
			operator = "<="
		}
		return c.compilePhotoDecisionPredicate("pf.rating"+operator+"?", n), nil
	case "flag", "label":
		if err := query.ValidateTextOperand(field, value); err != nil {
			return compiledQueryFragment{}, err
		}
		return c.compilePhotoDecisionPredicate("pf."+field+"=?", value), nil
	case "set":
		if err := query.ValidateTextOperand(field, value); err != nil {
			return compiledQueryFragment{}, err
		}
		return compilePhotoAssetPredicate(`EXISTS (SELECT 1 FROM photo_set_members sm JOIN photo_sets ps ON ps.set_id=sm.set_id WHERE sm.asset_id=pa.asset_id AND sm.set_id=? AND ps.deleted_at IS NULL)`, value), nil
	case "kind", "asset":
		if err := query.ValidateTextOperand(field, value); err != nil {
			return compiledQueryFragment{}, err
		}
		column := "pa.kind"
		if field == "asset" {
			column = "pa.asset_id"
		}
		return compilePhotoAssetPredicate(column+`=?`, value), nil
	case "camera", "lens":
		if err := query.ValidateTextOperand(field, value); err != nil {
			return compiledQueryFragment{}, err
		}
		folded := cases.Fold().String(value)
		return c.compilePhotoMetadataPredicate(`(p.`+field+`_make_folded=? OR p.`+field+`_model_folded=?)`, folded, folded), nil
	case "iso", "iso_min", "iso_max":
		n, err := query.ParseSizeOperand(value)
		if err != nil {
			return compiledQueryFragment{}, fmt.Errorf("ISO %s: %w", field, err)
		}
		operator := "="
		if field == "iso_min" {
			operator = ">="
		}
		if field == "iso_max" {
			operator = "<="
		}
		return c.compilePhotoMetadataPredicate(`p.iso `+operator+` ?`, n), nil
	case "capture_after", "capture_before":
		_, err := query.CaptureDateKey(value)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		operator := ">="
		if field == "capture_before" {
			operator = "<"
		}
		return c.compilePhotoMetadataPredicate(`p.capture_date<>'' AND p.capture_date`+operator+` ?`, value), nil
	case "gps":
		bounds, err := query.ParseGPSOperand(value)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		return c.compilePhotoGPSPredicate(bounds), nil
	default:
		return compiledQueryFragment{}, compileExpressionError(0, len(value), "unsupported photo operand")
	}
}

func (c queryCompiler) compilePhotoGPSPredicate(bounds query.GPSBounds) compiledQueryFragment {
	south, _ := strconv.ParseFloat(bounds.South, 64)
	west, _ := strconv.ParseFloat(bounds.West, 64)
	north, _ := strconv.ParseFloat(bounds.North, 64)
	east, _ := strconv.ParseFloat(bounds.East, 64)
	operator := ` AND `
	if west > east {
		operator = ` OR `
	}
	return c.compilePhotoMetadataPredicate(`p.latitude>=? AND p.latitude<=? AND (p.longitude>=?`+operator+`p.longitude<=?)`, south, north, west, east)
}

func (c queryCompiler) compilePhotoFilters(filters query.Filters, start, end int) (compiledQueryFragment, error) {
	parts := []compiledQueryFragment{}
	for _, bound := range query.QualityBounds(filters) {
		if bound.Value != nil {
			part, err := c.compilePhotoQualityPredicate(bound.Field, *bound.Value)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	if filters.Unevaluated {
		part, err := c.compilePhotoQualityPredicate("unevaluated", "true")
		if err != nil {
			return compiledQueryFragment{}, err
		}
		parts = append(parts, part)
	}
	for _, bound := range []struct {
		field string
		value *int64
	}{{"rating_min", filters.RatingMin}, {"rating_max", filters.RatingMax}} {
		if bound.value != nil {
			part, err := c.compileScalarPredicate(bound.field, strconv.FormatInt(*bound.value, 10), start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	for _, set := range []struct {
		field  string
		values []string
	}{{"flag", filters.Flags}, {"label", filters.Labels}, {"kind", filters.Kinds}, {"camera", filters.Cameras}, {"lens", filters.Lenses}, {"asset", filters.AssetIDs}, {"set", filters.SetIDs}} {
		matches := []compiledQueryFragment{}
		for _, v := range set.values {
			part, err := c.compileScalarPredicate(set.field, v, start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			matches = append(matches, part)
		}
		parts = append(parts, joinCompiledFragments(matches, ` OR `))
	}
	for _, bound := range []struct {
		field string
		value *int64
	}{{"iso_min", filters.ISOMin}, {"iso_max", filters.ISOMax}} {
		if bound.value != nil {
			part, err := c.compileScalarPredicate(bound.field, strconv.FormatInt(*bound.value, 10), start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	for _, bound := range []struct{ field, value string }{{"capture_after", filters.CaptureAfter}, {"capture_before", filters.CaptureBefore}} {
		if bound.value != "" {
			part, err := c.compileScalarPredicate(bound.field, bound.value, start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	if filters.GPSBounds != nil {
		b := filters.GPSBounds
		part, err := c.compileScalarPredicate("gps", strings.Join([]string{b.South, b.West, b.North, b.East}, ","), start, end)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		parts = append(parts, part)
	}
	return joinCompiledFragments(parts, ` AND `), nil
}

func (c queryCompiler) compilePhotoQualityPredicate(field, value string) (compiledQueryFragment, error) {
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return compiledQueryFragment{}, err
	}
	quality := func(alias string) string {
		return `EXISTS (SELECT 1 FROM photo_quality_signals q WHERE q.content_version_id=` + alias + `.version_id
 AND q.evaluator_fingerprint=? AND q.state='ready'`
	}
	args := []any{fingerprints.Evaluator}
	if field == "unevaluated" {
		return c.compilePhotoVersionPredicate(func(alias string) string {
			return liveIncludedPhotoDisplayPredicate(alias) + ` AND NOT ` + quality(alias) + `)`
		}, args...), nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return compiledQueryFragment{}, fmt.Errorf("parsing quality bound %s: %w", field, err)
	}
	column, operator := strings.TrimSuffix(field, "_min"), ">="
	if maxColumn, ok := strings.CutSuffix(field, "_max"); ok {
		column = maxColumn
		operator = "<="
	}
	args = append(args, number)
	return c.compilePhotoVersionPredicate(func(alias string) string {
		return quality(alias) + ` AND q.` + column + operator + ` ?)`
	}, args...), nil
}
