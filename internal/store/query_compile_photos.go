package store

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/text/cases"

	"go.kenn.io/docbank/internal/query"
)

func compilePhotoAssetPredicate(predicate string, args ...any) compiledQueryFragment {
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files pf JOIN photo_assets pa ON pa.asset_id=pf.asset_id WHERE pf.node_id=n.id AND ` + predicate + `)`, args: args}
}

func (c queryCompiler) compilePhotoMetadataPredicate(predicate string, args ...any) compiledQueryFragment {
	if c.photoDisplayMetadata {
		return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member
 JOIN photo_assets asset ON asset.asset_id=member.asset_id
 JOIN photo_files display ON display.file_id=asset.display_file_id
 JOIN nodes display_node ON display_node.id=display.node_id
 JOIN content_versions display_version ON display_version.version_id=display_node.current_version_id
 JOIN source_metadata_heads h ON h.source_sha256=display_version.blob_hash
 JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
 WHERE member.node_id=n.id AND ` + predicate + `)`, args: args}
	}
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM source_metadata_heads h JOIN photo_technical_metadata p ON p.generation_id=h.generation_id WHERE h.source_sha256=cv.blob_hash AND ` + predicate + `)`, args: args}
}

func (c queryCompiler) compilePhotoScalarPredicate(field, value string) (compiledQueryFragment, error) {
	switch field {
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
	for _, set := range []struct {
		field  string
		values []string
	}{{"kind", filters.Kinds}, {"camera", filters.Cameras}, {"lens", filters.Lenses}, {"asset", filters.AssetIDs}} {
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
