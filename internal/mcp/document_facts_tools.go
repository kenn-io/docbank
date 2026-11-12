package mcp

import (
	"context"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type documentFactsInput struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
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
			return documentDatesOutput{privateCache: newPrivateCache(), VersionDates: *result}, nil
		}
		d := apiclient.GetVersionPeopleQueryDisclosure("safe")
		result, err := c.API().GetVersionPeople(ctx, &apiclient.GetVersionPeopleRequestOptions{PathParams: &apiclient.GetVersionPeoplePath{VersionID: version}, Query: &apiclient.GetVersionPeopleQuery{Disclosure: &d}})
		if err != nil {
			return nil, err
		}
		return documentPeopleOutput{privateCache: newPrivateCache(), VersionPeople: *result}, nil
	})
}
