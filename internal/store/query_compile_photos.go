package store

import (
	"go.kenn.io/docbank/internal/query"
	"strconv"
	"strings"
)

const photoCaptureKeySQL = `docbank_query_capture_time_v1(COALESCE(p.capture_time,''),COALESCE(p.capture_time_precision,''),COALESCE(p.capture_time_timezone,''),COALESCE(p.capture_time_offset,''))`

func compilePhotoAssetPredicate(predicate string, args ...any) compiledQueryFragment {
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files pf JOIN photo_assets pa ON pa.asset_id=pf.asset_id WHERE pf.node_id=n.id AND ` + predicate + `)`, args: args}
}

func compilePhotoMetadataPredicate(predicate string, args ...any) compiledQueryFragment {
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM source_metadata_heads h JOIN photo_technical_metadata p ON p.generation_id=h.generation_id WHERE h.source_sha256=cv.blob_hash AND ` + predicate + `)`, args: args}
}

func compilePhotoScalarPredicate(field, value string) (compiledQueryFragment, error) {
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
		return compilePhotoMetadataPredicate(`(p.`+field+`_make=? OR p.`+field+`_model=?)`, value, value), nil
	case "iso", "iso_min", "iso_max":
		n, err := query.ParseSizeOperand(value)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		operator := "="
		if field == "iso_min" {
			operator = ">="
		}
		if field == "iso_max" {
			operator = "<="
		}
		return compilePhotoMetadataPredicate(`p.iso `+operator+` ?`, n), nil
	case "capture_after", "capture_before":
		key, err := query.CaptureDateKey(value)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		operator := ">="
		if field == "capture_before" {
			operator = "<"
		}
		return compilePhotoMetadataPredicate(photoCaptureKeySQL+`<>'' AND `+photoCaptureKeySQL+operator+` ?`, key), nil
	case "gps":
		bounds, err := query.ParseGPSOperand(value)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		return compilePhotoGPSPredicate(bounds), nil
	default:
		return compiledQueryFragment{}, compileExpressionError(0, len(value), "unsupported photo operand")
	}
}

func compilePhotoGPSPredicate(bounds query.GPSBounds) compiledQueryFragment {
	south, _ := strconv.ParseFloat(bounds.South, 64)
	west, _ := strconv.ParseFloat(bounds.West, 64)
	north, _ := strconv.ParseFloat(bounds.North, 64)
	east, _ := strconv.ParseFloat(bounds.East, 64)
	operator := ` AND `
	if west > east {
		operator = ` OR `
	}
	return compilePhotoMetadataPredicate(`p.latitude>=? AND p.latitude<=? AND (p.longitude>=?`+operator+`p.longitude<=?)`, south, north, west, east)
}

func compilePhotoFilters(filters query.Filters, start, end int) (compiledQueryFragment, error) {
	parts := []compiledQueryFragment{}
	for _, set := range []struct {
		field  string
		values []string
	}{{"kind", filters.Kinds}, {"camera", filters.Cameras}, {"lens", filters.Lenses}, {"asset", filters.AssetIDs}} {
		matches := []compiledQueryFragment{}
		for _, v := range set.values {
			part, err := compileScalarPredicate(set.field, v, start, end)
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
			part, err := compileScalarPredicate(bound.field, strconv.FormatInt(*bound.value, 10), start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	for _, bound := range []struct{ field, value string }{{"capture_after", filters.CaptureAfter}, {"capture_before", filters.CaptureBefore}} {
		if bound.value != "" {
			part, err := compileScalarPredicate(bound.field, bound.value, start, end)
			if err != nil {
				return compiledQueryFragment{}, err
			}
			parts = append(parts, part)
		}
	}
	if filters.GPSBounds != nil {
		b := filters.GPSBounds
		part, err := compileScalarPredicate("gps", strings.Join([]string{b.South, b.West, b.North, b.East}, ","), start, end)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		parts = append(parts, part)
	}
	return joinCompiledFragments(parts, ` AND `), nil
}
