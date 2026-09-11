package api

import (
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
	"reflect"
)

// CoverageCounts partitions current collection members by retained searchable output.
type CoverageCounts struct {
	Complete    int64 `json:"complete" minimum:"0"`
	Partial     int64 `json:"partial" minimum:"0"`
	Failed      int64 `json:"failed" minimum:"0"`
	Unprocessed int64 `json:"unprocessed" minimum:"0"`
	None        int64 `json:"none" minimum:"0"`
}

// ProcessingCoverage separates policy selection from output; it is not runtime readiness.
type ProcessingCoverage struct {
	Configuration      string          `json:"configuration" enum:"configured,unconfigured,profile_required"`
	Profile            string          `json:"profile"`
	Profiles           []string        `json:"profiles" maxItems:"64"`
	ProfileFingerprint string          `json:"profile_fingerprint"`
	GenerationID       string          `json:"generation_id"`
	Counts             *CoverageCounts `json:"counts"`
}

// Schema makes the unavailable-state null explicit. Huma's automatic pointer
// nullability only covers scalars, not object references.
func (ProcessingCoverage) Schema(r huma.Registry) *huma.Schema {
	type coverageSchema ProcessingCoverage
	schema := huma.SchemaFromType(r, reflect.TypeFor[coverageSchema]())
	schema.Properties["counts"] = &huma.Schema{AnyOf: []*huma.Schema{
		r.Schema(reflect.TypeFor[CoverageCounts](), true, ""),
		{Type: "null"},
	}}
	return schema
}

type QualityBucket struct {
	Value string `json:"value"`
	Count int64  `json:"count" minimum:"0"`
}
type QualityDimension struct {
	Field   string          `json:"field"`
	Values  []QualityBucket `json:"values" maxItems:"50"`
	Missing int64           `json:"missing" minimum:"0"`
	Other   int64           `json:"other" minimum:"0"`
}
type QualitySpike struct {
	Field string `json:"field"`
	Value string `json:"value"`
	Count int64  `json:"count" minimum:"0"`
}

// CollectionQuality is a bounded census from the collection coverage snapshot.
type CollectionQuality struct {
	Collection         Collection         `json:"collection"`
	SourceFingerprint  string             `json:"source_fingerprint"`
	Dimensions         []QualityDimension `json:"dimensions" maxItems:"7"`
	ZeroBytes          int64              `json:"zero_bytes" minimum:"0"`
	Mismatches         int64              `json:"mismatches" minimum:"0"`
	DuplicateDocuments int64              `json:"duplicate_documents" minimum:"0"`
	Spikes             []QualitySpike     `json:"spikes" maxItems:"7"`
}

func fromStoreCoverage(value store.ProcessingCoverage, selected ...collectionProfileSelection) ProcessingCoverage {
	out := ProcessingCoverage{Configuration: value.Configuration, ProfileFingerprint: value.ProfileFingerprint, GenerationID: value.GenerationID, Profiles: []string{}}
	if out.Configuration == "" {
		out.Configuration = "unconfigured"
	}
	if value.Counts != nil {
		c := value.Counts
		out.Counts = &CoverageCounts{Complete: c.Complete, Partial: c.Partial, Failed: c.Failed, Unprocessed: c.Unprocessed, None: c.None}
	}
	if len(selected) > 0 {
		out.Profile = selected[0].Name
		out.Profiles = selected[0].Names
	}
	return out
}

func fromStoreCollectionQuality(value store.CollectionQuality, selected collectionProfileSelection) CollectionQuality {
	out := CollectionQuality{Collection: fromStoreCollection(value.Collection, selected), SourceFingerprint: value.SourceFingerprint,
		Dimensions: []QualityDimension{}, ZeroBytes: value.ZeroBytes, Mismatches: value.Mismatches, DuplicateDocuments: value.DuplicateDocuments, Spikes: []QualitySpike{}}
	for _, dimension := range value.Dimensions {
		d := QualityDimension{Field: dimension.Field, Values: []QualityBucket{}, Missing: dimension.Missing, Other: dimension.Other}
		for _, b := range dimension.Values {
			d.Values = append(d.Values, QualityBucket{Value: b.Value, Count: b.Count})
		}
		out.Dimensions = append(out.Dimensions, d)
	}
	for _, s := range value.Spikes {
		out.Spikes = append(out.Spikes, QualitySpike{Field: s.Field, Value: s.Value, Count: s.Count})
	}
	return out
}
