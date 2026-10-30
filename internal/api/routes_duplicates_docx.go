package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// DOCXDuplicateGroup preserves each original's raw identity within a package match.
type DOCXDuplicateGroup struct {
	Fingerprint         string `json:"fingerprint" pattern:"^[0-9a-f]{64}$"`
	ReferenceCount      int    `json:"reference_count" minimum:"2"`
	References          []Node `json:"references" maxItems:"16"`
	ReferencesTruncated bool   `json:"references_truncated"`
}

type DOCXDuplicatePage struct {
	Items                     []DOCXDuplicateGroup `json:"items" maxItems:"100"`
	Total                     int                  `json:"total" minimum:"0"`
	TotalReferences           int                  `json:"total_references" minimum:"0"`
	UnfingerprintedReferences int                  `json:"unfingerprinted_references" minimum:"0"`
	Limit                     int                  `json:"limit" minimum:"1" maximum:"100"`
	Offset                    int                  `json:"offset" minimum:"0"`
}

func registerDOCXDuplicateRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "listDOCXDuplicates", Method: http.MethodGet, Path: "/api/v1/duplicates/docx",
		Summary:     "Find current DOCX files with identical package contents",
		Description: "Ignores ZIP packaging only. Groups are ordered by the v1 package fingerprint; references retain raw SHA-256 and size. Counts exclude trash and history. Unfingerprinted references include pending, unsupported, or failed inspection. No originals are deleted or merged.",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"50" minimum:"1" maximum:"100"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*struct{ Body DOCXDuplicatePage }, error) {
		page, err := d.Store.DOCXDuplicates(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := DOCXDuplicatePage{Items: []DOCXDuplicateGroup{}, Total: page.Total,
			TotalReferences: page.TotalReferences, UnfingerprintedReferences: page.UnfingerprintedReferences,
			Limit: page.Limit, Offset: page.Offset}
		for _, group := range page.Items {
			item := DOCXDuplicateGroup{Fingerprint: group.Fingerprint, ReferenceCount: group.ReferenceCount,
				References: []Node{}, ReferencesTruncated: group.ReferencesTruncated}
			for _, ref := range group.References {
				node := fromStoreNode(ref.Node)
				node.Path = ref.Path
				item.References = append(item.References, node)
			}
			out.Items = append(out.Items, item)
		}
		return &struct{ Body DOCXDuplicatePage }{Body: out}, nil
	})
}
