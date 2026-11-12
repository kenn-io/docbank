package mcp

import (
	"context"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type documentFactsInput struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Offset           int    `json:"offset"`
}

func documentFactsSelection(raw []byte) (documentFactsInput, error) {
	var in documentFactsInput
	if err := decodeReadArguments(raw, &in); err != nil {
		return in, err
	}
	if (in.NodeID > 0) == (in.ContentVersionID != "") || in.NodeID < 0 || in.ContentVersionID != "" && !daemonconn.IsCanonicalUUIDv4(in.ContentVersionID) {
		return in, invalidToolArgumentsError()
	}
	return in, nil
}

type documentDatesOutput struct {
	privateCache
	api.VersionDates
	Offset     int  `json:"offset"`
	NextOffset *int `json:"next_offset,omitzero"`
}
type documentPeopleOutput struct {
	privateCache
	api.VersionPeople
}

func getDocumentFacts(ctx context.Context, lease *daemonLease, raw []byte, kind string) (any, error) {
	in, err := documentFactsSelection(raw)
	if err != nil {
		return nil, err
	}
	return daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (any, error) {
		id := in.ContentVersionID
		if in.NodeID > 0 {
			node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: in.NodeID}})
			if err != nil {
				return nil, err
			}
			id = node.CurrentVersionID
			if id == "" {
				return nil, &searchProfileError{code: "no_content_version", message: "Document has no current content version."}
			}
		}
		version, err := uuid.Parse(id)
		if err != nil {
			return nil, invalidToolArgumentsError()
		}
		if kind == "dates" {
			d := apiclient.GetVersionDatesQueryDisclosure("safe")
			result, err := c.API().GetVersionDates(ctx, &apiclient.GetVersionDatesRequestOptions{PathParams: &apiclient.GetVersionDatesPath{VersionID: version}, Query: &apiclient.GetVersionDatesQuery{Disclosure: &d}})
			if err != nil {
				return nil, err
			}
			return documentDatesPage(*result, in.Offset)
		}
		d := apiclient.GetVersionPeopleQueryDisclosure("safe")
		result, err := c.API().GetVersionPeople(ctx, &apiclient.GetVersionPeopleRequestOptions{PathParams: &apiclient.GetVersionPeoplePath{VersionID: version}, Query: &apiclient.GetVersionPeopleQuery{Disclosure: &d}})
		if err != nil {
			return nil, err
		}
		return documentPeopleOutput{privateCache: newPrivateCache(), VersionPeople: *result}, nil
	})
}

// documentDatesPage pages claims followed by diagnostics, retaining the primary
// and version metadata on every page. Oversized individual items fail visibly.
func documentDatesPage(dates api.VersionDates, offset int) (documentDatesOutput, error) {
	const maxItems = document.MaxDocumentEvents + document.MaxDocumentEventsEncodedBytes/32
	if offset < 0 || offset > maxItems {
		return documentDatesOutput{}, invalidToolArgumentsError()
	}
	total := len(dates.Claims) + len(dates.Diagnostics)
	start := min(offset, total)
	output := documentDatesOutput{privateCache: newPrivateCache(), VersionDates: dates, Offset: start}
	output.Claims = []api.VersionDateClaim{}
	output.Diagnostics = []api.FactDiagnostic{}
	const budget = maxToolResponseBytes - 8<<10
	for i := start; i < min(total, start+100); i++ {
		if i < len(dates.Claims) {
			output.Claims = append(output.Claims, dates.Claims[i])
		} else {
			output.Diagnostics = append(output.Diagnostics, dates.Diagnostics[i-len(dates.Claims)])
		}
		if i+1 < total {
			output.NextOffset = new(i + 1)
		} else {
			output.NextOffset = nil
		}
		size, err := toolEnvelopeBytes(output, nil)
		if err != nil {
			return documentDatesOutput{}, err
		}
		if size > budget {
			if i == start {
				return documentDatesOutput{}, errToolResultTooLarge
			}
			if i < len(dates.Claims) {
				output.Claims = output.Claims[:len(output.Claims)-1]
			} else {
				output.Diagnostics = output.Diagnostics[:len(output.Diagnostics)-1]
			}
			output.NextOffset = new(i)
			break
		}
	}
	return output, nil
}
