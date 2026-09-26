package store

import (
	"context"
	"strings"

	"go.kenn.io/docbank/internal/query"
)

// ContentMapProposalRequest names an existing tag or an explicitly scoped
// query. Proposals are read-only; callers must accept the digest to save one.
type ContentMapProposalRequest struct {
	TagID string       `json:"tag_id,omitzero"`
	Title string       `json:"title,omitzero"`
	Query *query.Query `json:"query,omitzero"`
}

func (s *Store) ProposeContentMap(ctx context.Context, access MapAccess,
	request ContentMapProposalRequest,
) (ContentMapPlan, error) {
	if (request.TagID == "") == (request.Query == nil) {
		return ContentMapPlan{}, ErrInvalidContentMap
	}
	var title, scope string
	var selector query.Query
	if request.TagID != "" {
		if request.Title != "" {
			return ContentMapPlan{}, ErrInvalidContentMap
		}
		tag, err := s.TagByID(ctx, request.TagID)
		if err != nil {
			return ContentMapPlan{}, err
		}
		title, scope = tag.Name, "tag:"+tag.ID
		selector = query.Query{V: 1, Syntax: "simple", Mode: "lexical",
			Sort:    query.Sort{Field: string(DocumentCatalogSortName), Direction: "asc"},
			Filters: query.Filters{TagIDs: []string{tag.ID}}}
	} else {
		title = strings.TrimSpace(request.Title)
		selector = *request.Query
		if title == "" || (strings.TrimSpace(selector.Text) == "" &&
			len(selector.Filters.TagIDs) == 0 && len(selector.Filters.Paths) == 0 &&
			len(selector.Filters.CollectionIDs) == 0) {
			return ContentMapPlan{}, ErrInvalidContentMap
		}
		scope = "query"
	}
	return s.PreviewContentMap(ctx, access, ContentMapDefinition{Title: title, Scope: scope,
		Sections: []ContentMapSection{{ID: "content", Heading: title, Selector: &selector,
			Ordering: "explicit", MaxEntries: 100}}})
}
