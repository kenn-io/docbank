package daemonconn

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

type ProductionNumberQuery struct {
	Label         string
	NamespaceID   string
	StartSequence int64
	EndSequence   int64
	AfterSequence int64
	Limit         int
}

func (c *Connection) ProductionNumberCandidates(ctx context.Context,
	query string, limit int) (api.ProductionNumberCandidates, error) {
	if query == "" || len(query) > 256 || !utf8.ValidString(query) || strings.TrimSpace(query) != query ||
		limit < 0 || limit > 25 {
		return api.ProductionNumberCandidates{}, errors.New("invalid production number candidate query")
	}
	requestedLimit := limit
	if requestedLimit == 0 {
		requestedLimit = 25
	}
	request := apiclient.FindProductionNumberCandidatesQuery{Query: &query}
	if limit != 0 {
		value := int64(limit)
		request.Limit = &value
	}
	result, err := c.API().FindProductionNumberCandidates(ctx,
		&apiclient.FindProductionNumberCandidatesRequestOptions{Query: &request})
	if err != nil {
		return api.ProductionNumberCandidates{}, err
	}
	if result == nil || len(result.Items) > requestedLimit || result.Truncated &&
		(len(result.Items) != requestedLimit || !result.Ambiguous) ||
		!result.Truncated && result.Ambiguous != (len(result.Items) > 1) {
		return api.ProductionNumberCandidates{}, integrityErrorf("production number candidates are inconsistent")
	}
	if result.MatchKind == "none" && len(result.Items) == 0 && !result.Ambiguous && !result.Truncated {
		return *result, nil
	}
	if result.MatchKind != "exact" && result.MatchKind != "prefix" && result.MatchKind != "substring" ||
		len(result.Items) == 0 || result.MatchKind == "exact" &&
		(len(result.Items) != 1 || result.Ambiguous || result.Truncated) {
		return api.ProductionNumberCandidates{}, integrityErrorf("production number match kind is inconsistent")
	}
	previous := ""
	for _, item := range result.Items {
		if item.Label <= previous || result.MatchKind == "exact" && item.Label != query ||
			result.MatchKind == "prefix" && !strings.HasPrefix(item.Label, query) ||
			result.MatchKind == "substring" && !strings.Contains(item.Label, query) {
			return api.ProductionNumberCandidates{}, integrityErrorf("production number candidate is inconsistent")
		}
		previous = item.Label
	}
	return *result, nil
}

// ProductionNumbers reads exact or ranged published production number
// references through the authenticated daemon API.
func (c *Connection) ProductionNumbers(ctx context.Context, query ProductionNumberQuery) (api.ProductionNumberPage, error) {
	ranged := query.NamespaceID != "" || query.StartSequence != 0 || query.EndSequence != 0 ||
		query.AfterSequence != 0
	if (query.Label == "") == !ranged || query.Label != "" && query.Limit != 0 ||
		query.Limit < 0 || query.Limit > 25 {
		return api.ProductionNumberPage{}, errors.New("exact label or production number range is required")
	}
	request := apiclient.FindProductionNumbersQuery{}
	if query.Label != "" {
		request.Label = &query.Label
	} else {
		request.NamespaceID = &query.NamespaceID
		request.StartSequence = &query.StartSequence
		request.EndSequence = &query.EndSequence
		if query.AfterSequence != 0 {
			request.AfterSequence = &query.AfterSequence
		}
		if query.Limit != 0 {
			limit := int64(query.Limit)
			request.Limit = &limit
		}
	}
	result, err := c.API().FindProductionNumbers(ctx, &apiclient.FindProductionNumbersRequestOptions{Query: &request})
	if err != nil {
		return api.ProductionNumberPage{}, err
	}
	if result == nil || len(result.Items) > 25 || query.Label != "" &&
		(len(result.Items) != 1 || result.Items[0].Label != query.Label || result.NextSequence != 0) {
		return api.ProductionNumberPage{}, integrityErrorf("production number response is inconsistent")
	}
	return *result, nil
}
