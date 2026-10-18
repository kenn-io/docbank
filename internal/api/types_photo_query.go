package api

import (
	"errors"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// PhotoBrowseRequest carries strict QueryV1 JSON and live-page options.
type PhotoBrowseRequest struct {
	Projection string                 `json:"projection,omitempty" enum:"map"`
	Hidden     bool                   `json:"hidden,omitempty"`
	Query      QueryPayload           `json:"query"`
	Coverage   WorkspaceQueryCoverage `json:"coverage,omitzero"`
	PageSize   int                    `json:"page_size,omitempty,omitzero" minimum:"1" default:"50" doc:"Maximum 1000 only with projection: \"map\"; 250 otherwise."`
	Facets     []string               `json:"facets,omitempty" maxItems:"8" uniqueItems:"true" enum:"camera,lens,year,location,set,rating,flag,label"`
	Cursor     string                 `json:"cursor,omitempty" maxLength:"32768"`
}

func (PhotoBrowseRequest) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.Properties["page_size"].Maximum = new(float64(store.MaxPhotoMapPageSize))
	return schema
}

type PhotoPreviewSlot struct {
	State        string `json:"state" enum:"missing,ready,unsupported,failed"`
	GenerationID string `json:"generation_id,omitempty" pattern:"^[0-9a-f]{64}$"`
	URL          string `json:"url,omitempty"`
	SHA256       string `json:"sha256,omitempty" pattern:"^[0-9a-f]{64}$"`
	Size         *int64 `json:"size,omitempty" minimum:"1"`
	MediaType    string `json:"media_type,omitempty" enum:"image/jpeg"`
	Width        *int   `json:"width,omitempty" minimum:"1"`
	Height       *int   `json:"height,omitempty" minimum:"1"`
}

type PhotoPreviewSlots struct {
	Grid  PhotoPreviewSlot `json:"grid"`
	Fit   PhotoPreviewSlot `json:"fit"`
	Large PhotoPreviewSlot `json:"large"`
}

type PhotoQuality struct {
	State   string                        `json:"state" enum:"pending,unavailable,ready"`
	Signals *document.PhotoQualitySignals `json:"signals"`
}

type PhotoBrowseRow struct {
	Rating               int               `json:"rating" minimum:"0" maximum:"5"`
	Flag                 string            `json:"flag" enum:",pick,reject"`
	Quality              *PhotoQuality     `json:"quality,omitempty"`
	AssetID              string            `json:"asset_id" format:"uuid"`
	Kind                 string            `json:"kind" enum:"photo,video"`
	Revision             int64             `json:"revision" minimum:"1"`
	DisplayFileID        string            `json:"display_file_id" format:"uuid"`
	Rotation             int               `json:"rotation" enum:"0,90,180,270"`
	NodeID               int64             `json:"node_id" minimum:"1"`
	ContentVersionID     string            `json:"content_version_id" format:"uuid"`
	Name                 string            `json:"name"`
	MediaType            string            `json:"media_type"`
	ImportTime           string            `json:"import_time"`
	CaptureTime          *string           `json:"capture_time" nullable:"true"`
	FileModifiedTime     *string           `json:"file_modified_time" nullable:"true" format:"date-time" doc:"Set only when the file modified time, not a capture date, placed the photo."`
	CaptureTimePrecision *string           `json:"capture_time_precision" nullable:"true"`
	CaptureTimeTimezone  *string           `json:"capture_time_timezone" nullable:"true"`
	CaptureTimeOffset    *string           `json:"capture_time_offset" nullable:"true"`
	WidthPX              *int64            `json:"width_px" nullable:"true"`
	HeightPX             *int64            `json:"height_px" nullable:"true"`
	CameraMake           *string           `json:"camera_make" nullable:"true"`
	CameraModel          *string           `json:"camera_model" nullable:"true"`
	LensMake             *string           `json:"lens_make" nullable:"true"`
	LensModel            *string           `json:"lens_model" nullable:"true"`
	ISO                  *int64            `json:"iso" nullable:"true"`
	ExposureTimeSeconds  *float64          `json:"exposure_time_seconds" nullable:"true"`
	FNumber              *float64          `json:"f_number" nullable:"true"`
	ExposureBiasEV       *float64          `json:"exposure_bias_ev" nullable:"true"`
	FocalLengthMM        *float64          `json:"focal_length_mm" nullable:"true"`
	Orientation          *int64            `json:"orientation" nullable:"true"`
	Previews             PhotoPreviewSlots `json:"previews"`
}

type PhotoMapRow struct {
	Rotation         int      `json:"rotation" enum:"0,90,180,270"`
	AssetID          string   `json:"asset_id" format:"uuid"`
	Revision         int64    `json:"revision" minimum:"1"`
	Name             string   `json:"name"`
	CaptureTime      *string  `json:"capture_time" nullable:"true"`
	FileModifiedTime *string  `json:"file_modified_time" nullable:"true" format:"date-time" doc:"Set only when the file modified time, not a capture date, placed the photo."`
	WidthPX          *int64   `json:"width_px" nullable:"true"`
	HeightPX         *int64   `json:"height_px" nullable:"true"`
	Latitude         *float64 `json:"latitude" nullable:"true"`
	Longitude        *float64 `json:"longitude" nullable:"true"`
	Previews         struct {
		Grid PhotoPreviewSlot `json:"grid"`
	} `json:"previews"`
}

type PhotoBrowsePage struct {
	MapItems           []PhotoMapRow    `json:"map_items,omitempty"`
	Items              []PhotoBrowseRow `json:"items"`
	Total              int64            `json:"total" minimum:"0"`
	Facets             []WorkspaceFacet `json:"facets"`
	NextCursor         string           `json:"next_cursor,omitempty"`
	RelevanceAvailable bool             `json:"relevance_available"`
}

// Schema makes missing measurements nullable while sharing the signals definition.
func (PhotoQuality) Schema(r huma.Registry) *huma.Schema {
	type qualitySchema PhotoQuality
	schema := huma.SchemaFromType(r, reflect.TypeFor[qualitySchema]())
	schema.Properties["signals"] = &huma.Schema{AnyOf: []*huma.Schema{
		r.Schema(reflect.TypeFor[document.PhotoQualitySignals](), true, ""),
		{Type: "null"},
	}}
	return schema
}

type PreparePhotoPreviewRequest struct {
	Hidden           bool   `json:"hidden,omitempty" default:"false"`
	ContentVersionID string `json:"content_version_id" format:"uuid"`
	Size             string `json:"size" enum:"fit,large"`
}

// PhotoTimelineRequest selects the photos to count by capture day. It takes the
// same query and coverage as photo browsing, so counts match the browsed photos.
type PhotoTimelineRequest struct {
	Query    QueryPayload           `json:"query"`
	Coverage WorkspaceQueryCoverage `json:"coverage,omitzero"`
}

// PhotoCaptureDayFacet counts photo assets by recorded calendar day. It allows
// one value per day, up to the snapshot member limit.
type PhotoCaptureDayFacet WorkspaceFacet

// Schema reuses the nullable facet counts and widens the value bound to one
// value per possible day.
func (PhotoCaptureDayFacet) Schema(r huma.Registry) *huma.Schema {
	schema := (WorkspaceFacet{}).Schema(r)
	schema.Properties["dimension"].Enum = []any{"capture_day"}
	schema.Properties["values"].MaxItems = new(250000)
	return schema
}

// PhotoTimeline returns capture-day counts with the query evidence that
// produced them. It creates no snapshot, rows, or cursors.
type PhotoTimeline struct {
	Query        QueryPayload               `json:"query"`
	Dependencies []WorkspaceQueryDependency `json:"dependencies"`
	Generation   WorkspaceQueryGeneration   `json:"generation"`
	Coverage     WorkspaceQueryCoverage     `json:"coverage"`
	ObservedAt   time.Time                  `json:"observed_at" format:"date-time"`
	Facets       []PhotoCaptureDayFacet     `json:"facets" minItems:"1" maxItems:"1"`
}

func fromStorePhotoTimeline(projection store.SnapshotProjection) (PhotoTimeline, error) {
	wire, err := fromStoreWorkspacePage(store.SnapshotPage{SnapshotProjection: projection})
	if err != nil {
		return PhotoTimeline{}, err
	}
	if len(wire.Facets) != 1 {
		return PhotoTimeline{}, errors.New("photo timeline must contain exactly one facet")
	}
	return PhotoTimeline{
		Query: wire.Query, Dependencies: wire.Dependencies, Generation: wire.Generation,
		Coverage: wire.Coverage, ObservedAt: wire.ObservedAt,
		Facets: []PhotoCaptureDayFacet{PhotoCaptureDayFacet(wire.Facets[0])},
	}, nil
}
