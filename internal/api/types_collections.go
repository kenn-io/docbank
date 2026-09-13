package api

import "go.kenn.io/docbank/internal/store"

// Collection is one document-bearing ingest run and its current live summary.
// LabelRevision advances independently of member observations.
type Collection struct {
	ID                string  `json:"id" format:"uuid"`
	SourceKind        string  `json:"source_kind" minLength:"1"`
	SourceDescription string  `json:"source_description" minLength:"1"`
	StartedAt         string  `json:"started_at" format:"date-time"`
	FileCount         int64   `json:"file_count" minimum:"0"`
	TotalBytes        int64   `json:"total_bytes" minimum:"0"`
	Label             *string `json:"label"`
	LabelRevision     int64   `json:"label_revision" minimum:"1"`
	LabelUpdatedAt    string  `json:"label_updated_at" format:"date-time"`
}

// CollectionPage is one bounded, newest-first page of nonempty collections.
type CollectionPage struct {
	Items  []Collection `json:"items"`
	Total  int          `json:"total" minimum:"0"`
	Limit  int          `json:"limit" minimum:"1" maximum:"1000"`
	Offset int          `json:"offset" minimum:"0"`
}

// CollectionMemberPage binds live members and the collection summary to one
// read snapshot. Each member carries its current live path.
type CollectionMemberPage struct {
	Collection Collection `json:"collection"`
	Items      []Node     `json:"items"`
	Total      int        `json:"total" minimum:"0"`
	Limit      int        `json:"limit" minimum:"1" maximum:"1000"`
	Offset     int        `json:"offset" minimum:"0"`
}

// CollectionLabel is the independently revisioned label authority for one
// ingest run. A nil Label is the explicit cleared state.
type CollectionLabel struct {
	IngestID  string  `json:"ingest_id" format:"uuid"`
	Label     *string `json:"label"`
	Revision  int64   `json:"revision" minimum:"1"`
	UpdatedAt string  `json:"updated_at" format:"date-time"`
}

func fromStoreCollection(collection store.Collection) Collection {
	return Collection{
		ID: collection.ID, SourceKind: collection.SourceKind,
		SourceDescription: collection.SourceDescription, StartedAt: collection.StartedAt,
		FileCount: collection.FileCount, TotalBytes: collection.TotalBytes,
		Label: collection.Label, LabelRevision: collection.LabelRevision,
		LabelUpdatedAt: collection.LabelUpdatedAt,
	}
}

func fromStoreCollectionLabel(label store.CollectionLabel) CollectionLabel {
	return CollectionLabel{
		IngestID: label.IngestID, Label: label.Label,
		Revision: label.Revision, UpdatedAt: label.UpdatedAt,
	}
}
