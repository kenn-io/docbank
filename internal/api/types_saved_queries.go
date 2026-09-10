package api

import (
	"bytes"
	jsonv1 "encoding/json"
	"errors"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/query"
)

// The envelope allowance covers maximally escaped bounded name and description
// fields plus JSON member syntax around one maximum-sized payload.
const maxSavedQueryRequestBytes = query.MaxInputBytes + (32 << 10)

// SavedQueryPayload carries one complete query or literal highlight set as
// structured JSON. Its raw representation reaches the strict canonical codec
// unchanged, including duplicate member names and numeric lexemes.
type SavedQueryPayload jsonv1.RawMessage

func (p SavedQueryPayload) MarshalJSON() ([]byte, error) {
	if len(p) == 0 {
		return nil, errors.New("saved query payload is missing")
	}
	return p, nil
}

func (p *SavedQueryPayload) UnmarshalJSON(raw []byte) error {
	if len(raw) > query.MaxInputBytes {
		return errors.New("saved query payload exceeds 128 KiB")
	}
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("saved query payload must be an object")
	}
	*p = append((*p)[:0], raw...)
	return nil
}

func (SavedQueryPayload) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{
		r.Schema(reflect.TypeFor[savedQueryV1Schema](), true, ""),
		r.Schema(reflect.TypeFor[highlightSetV1Schema](), true, ""),
	}}
}

type savedQueryV1Schema struct {
	V       int                     `json:"v,omitempty" enum:"1" default:"1"`
	Text    string                  `json:"text,omitempty" maxLength:"8192"`
	Syntax  string                  `json:"syntax,omitempty" enum:"simple,advanced" default:"simple"`
	Mode    string                  `json:"mode,omitempty" enum:"lexical,semantic,hybrid" default:"lexical"`
	Filters savedQueryFiltersSchema `json:"filters,omitzero"`
	Sort    savedQuerySortSchema    `json:"sort,omitzero"`
}

type savedQuerySortSchema struct {
	Field     string `json:"field,omitempty" enum:"name,path,modified_at,size,media_type,relevance" default:"name"`
	Direction string `json:"direction,omitempty" enum:"asc,desc" default:"asc"`
}

type savedQueryFiltersSchema struct {
	Paths                []string `json:"paths,omitempty" maxItems:"64"`
	ExcludePaths         []string `json:"exclude_paths,omitempty" maxItems:"64"`
	CollectionIDs        []string `json:"collection_ids,omitempty" maxItems:"64" format:"uuid"`
	ExcludeCollectionIDs []string `json:"exclude_collection_ids,omitempty" maxItems:"64" format:"uuid"`
	TagIDs               []string `json:"tag_ids,omitempty" maxItems:"64" format:"uuid"`
	ExcludeTagIDs        []string `json:"exclude_tag_ids,omitempty" maxItems:"64" format:"uuid"`
	NoTags               *bool    `json:"no_tags,omitempty" nullable:"true"`
	MediaFamilies        []string `json:"media_families,omitempty" maxItems:"13" enum:"email,document,spreadsheet,presentation,image,audio_video,text,source_code,web,calendar,archive,cad,unknown"`
	MIMETypes            []string `json:"mime_types,omitempty" maxItems:"64"`
	Extensions           []string `json:"extensions,omitempty" maxItems:"32" pattern:"^[a-z0-9](?:[a-z0-9_-]{0,31})$"`
	ModifiedAfter        *string  `json:"modified_after,omitempty" nullable:"true" format:"date-time"`
	ModifiedBefore       *string  `json:"modified_before,omitempty" nullable:"true" format:"date-time"`
	SizeMin              *int64   `json:"size_min,omitempty" nullable:"true" minimum:"0" maximum:"9007199254740991"`
	SizeMax              *int64   `json:"size_max,omitempty" nullable:"true" minimum:"0" maximum:"9007199254740991"`
	TextCoverage         []string `json:"text_coverage,omitempty" maxItems:"6" enum:"complete,partial,failed,unprocessed,none,unavailable"`
	HasDuplicates        *bool    `json:"has_duplicates,omitempty" nullable:"true"`
	CollapseDuplicates   *bool    `json:"collapse_duplicates,omitempty" nullable:"true"`
}

type highlightSetV1Schema struct {
	V     int                        `json:"v,omitempty" enum:"1" default:"1"`
	Terms []highlightSetTermV1Schema `json:"terms" minItems:"1" maxItems:"64"`
}

type highlightSetTermV1Schema struct {
	Text  string `json:"text" minLength:"1" maxLength:"256"`
	Color string `json:"color" pattern:"^#[0-9a-f]{6}$"`
}

// SavedQuery is one named, revision-fenced query or literal highlight set.
type SavedQuery struct {
	ID          string            `json:"id" format:"uuid"`
	Name        string            `json:"name" minLength:"1" maxLength:"256"`
	Description string            `json:"description" maxLength:"4096"`
	Kind        string            `json:"kind" enum:"query,highlight_set"`
	Payload     SavedQueryPayload `json:"payload"`
	Fingerprint string            `json:"fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	Revision    int64             `json:"revision" minimum:"1"`
	CreatedAt   string            `json:"created_at" format:"date-time"`
	UpdatedAt   string            `json:"updated_at" format:"date-time"`
}

// SavedQueryPage is one consistent, bounded, name-sorted listing.
type SavedQueryPage struct {
	Items  []SavedQuery `json:"items"`
	Total  int          `json:"total" minimum:"0"`
	Limit  int          `json:"limit" minimum:"1" maximum:"1000"`
	Offset int          `json:"offset" minimum:"0"`
}

// SavedQueryCreateRequest defines one named query or literal highlight set.
type SavedQueryCreateRequest struct {
	Name        string            `json:"name" minLength:"1" doc:"Normalized to NFC; must contain 1 to 256 UTF-8 bytes after NFC normalization."`
	Description string            `json:"description,omitempty" maxLength:"4096"`
	Kind        string            `json:"kind" enum:"query,highlight_set"`
	Payload     SavedQueryPayload `json:"payload"`
}

// SavedQueryPatch replaces only the supplied mutable fields. Kind and all
// identity, revision, fingerprint, and timestamp fields are server-owned.
type SavedQueryPatch struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Payload     *SavedQueryPayload `json:"payload,omitempty"`
}
