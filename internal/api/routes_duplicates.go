package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// DuplicateCollection is a display label, not a replacement for ingest identity.
type DuplicateCollection struct {
	ID    string  `json:"id" format:"uuid"`
	Label *string `json:"label"`
}

// DuplicateReference describes one current document and its bounded import memberships.
type DuplicateReference struct {
	Reference            ContentReference      `json:"reference"`
	Collections          []DuplicateCollection `json:"collections" maxItems:"16"`
	CollectionCount      int                   `json:"collection_count" minimum:"0"`
	CollectionsTruncated bool                  `json:"collections_truncated"`
}

// DuplicateGroup groups live current document identities without deleting any of them.
type DuplicateGroup struct {
	SHA256               string               `json:"sha256" pattern:"^[0-9a-f]{64}$"`
	Size                 int64                `json:"size" minimum:"0"`
	ReferenceCount       int                  `json:"reference_count" minimum:"2"`
	RepresentativeNodeID int64                `json:"representative_node_id" minimum:"1"`
	References           []DuplicateReference `json:"references" maxItems:"16"`
	ReferencesTruncated  bool                 `json:"references_truncated"`
}

// DuplicatePage keeps exact population totals separate from bounded display previews.
type DuplicatePage struct {
	Items           []DuplicateGroup `json:"items" maxItems:"100"`
	Total           int              `json:"total" minimum:"0"`
	TotalReferences int              `json:"total_references" minimum:"0"`
	Limit           int              `json:"limit" minimum:"1" maximum:"100"`
	Offset          int              `json:"offset" minimum:"0"`
}

type DuplicateContextReference struct {
	NodeID     int64  `json:"node_id" minimum:"1"`
	Revision   int64  `json:"revision" minimum:"1"`
	VersionID  string `json:"version_id" format:"uuid"`
	SHA256     string `json:"sha256" pattern:"^[0-9a-f]{64}$"`
	Size       int64  `json:"size" minimum:"0"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	MediaType  string `json:"media_type"`
	ModifiedAt string `json:"modified_at" format:"date-time"`
}

type DuplicateContextGroup struct {
	SHA256              string                      `json:"sha256" pattern:"^[0-9a-f]{64}$"`
	Size                int64                       `json:"size" minimum:"0"`
	ReferenceCount      int                         `json:"reference_count" minimum:"2"`
	References          []DuplicateContextReference `json:"references" maxItems:"16"`
	ReferencesTruncated bool                        `json:"references_truncated"`
}

func registerDuplicateRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getDuplicateContentByHash", Method: http.MethodGet,
		Path: "/api/v1/duplicates/by-hash", Summary: "Read one exact live-current duplicate group",
	}, func(ctx context.Context, in *struct {
		SHA256 string `query:"sha256" pattern:"^[0-9a-f]{64}$"`
		Size   int64  `query:"size" minimum:"0"`
	}) (*struct{ Body DuplicateContextGroup }, error) {
		group, err := d.Store.DuplicateGroupByHash(ctx, in.SHA256, in.Size)
		if err != nil {
			return nil, FromStoreError(err)
		}
		body := DuplicateContextGroup{SHA256: group.Hash, Size: group.Size,
			ReferenceCount: group.ReferenceCount, References: []DuplicateContextReference{},
			ReferencesTruncated: group.ReferencesTruncated}
		for _, member := range group.References {
			body.References = append(body.References, DuplicateContextReference{
				NodeID: member.Reference.Node.ID, Revision: member.Reference.Node.Revision,
				VersionID: member.Reference.Version.ID, SHA256: member.Reference.Version.BlobHash,
				Size: member.Reference.Version.Size, Name: member.Reference.Node.Name,
				Path: member.Reference.Path, MediaType: member.Reference.Version.MimeType,
				ModifiedAt: member.Reference.Node.ModifiedAt,
			})
		}
		return &struct{ Body DuplicateContextGroup }{Body: body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listDuplicateContent", Method: http.MethodGet, Path: "/api/v1/duplicates",
		Summary:     "Find live documents that share current content",
		Description: "Groups are ordered by SHA-256. Counts exclude historical versions and trash; each group displays at most 16 current references. No content is deleted or merged.",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"50" minimum:"1" maximum:"100"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*struct{ Body DuplicatePage }, error) {
		page, err := d.Store.Duplicates(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body DuplicatePage }{Body: fromStoreDuplicatePage(page)}, nil
	})
}

func fromStoreDuplicatePage(page store.DuplicatePage) DuplicatePage {
	out := DuplicatePage{Items: []DuplicateGroup{}, Total: page.Total,
		TotalReferences: page.TotalReferences, Limit: page.Limit, Offset: page.Offset}
	for _, group := range page.Items {
		item := DuplicateGroup{SHA256: group.Hash, Size: group.Size,
			ReferenceCount: group.ReferenceCount, RepresentativeNodeID: group.RepresentativeNodeID,
			References: []DuplicateReference{}, ReferencesTruncated: group.ReferencesTruncated}
		for _, ref := range group.References {
			member := DuplicateReference{Reference: fromStoreContentReference(ref.Reference),
				Collections: []DuplicateCollection{}, CollectionCount: ref.CollectionCount,
				CollectionsTruncated: ref.CollectionsTruncated}
			for _, collection := range ref.Collections {
				member.Collections = append(member.Collections, DuplicateCollection{ID: collection.ID, Label: collection.Label})
			}
			item.References = append(item.References, member)
		}
		out.Items = append(out.Items, item)
	}
	return out
}
