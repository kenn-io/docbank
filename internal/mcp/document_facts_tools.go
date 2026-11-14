package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type documentFactsInput struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Offset           int    `json:"offset"`
	Snapshot         string `json:"snapshot"`
}

type documentDatesOutput struct {
	privateCache
	api.VersionDates

	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset,omitzero"`
	Snapshot   string `json:"snapshot"`
}
type documentPeopleOutput struct {
	privateCache
	api.VersionPeople

	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset,omitzero"`
	Snapshot   string `json:"snapshot"`
}

func getDocumentFacts(ctx context.Context, lease *daemonLease, raw []byte, kind string) (any, error) {
	var in documentFactsInput
	if err := decodeReadArguments(raw, &in); err != nil {
		return nil, err
	}
	type factsResponse struct {
		dates     *api.VersionDates
		people    *api.VersionPeople
		noVersion bool
	}
	response, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (factsResponse, error) {
		id := in.ContentVersionID
		if in.NodeID > 0 {
			node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: in.NodeID}})
			if err != nil {
				return factsResponse{}, err
			}
			id = node.CurrentVersionID
			if id == "" {
				return factsResponse{noVersion: true}, nil
			}
		}
		version, err := uuid.Parse(id)
		if err != nil {
			return factsResponse{}, fmt.Errorf("parse content version ID: %w", err)
		}
		if kind == "dates" {
			d := apiclient.GetVersionDatesQueryDisclosure("safe")
			result, err := c.API().GetVersionDates(ctx, &apiclient.GetVersionDatesRequestOptions{PathParams: &apiclient.GetVersionDatesPath{VersionID: version}, Query: &apiclient.GetVersionDatesQuery{Disclosure: &d}})
			return factsResponse{dates: result}, err
		}
		d := apiclient.GetVersionPeopleQueryDisclosure("safe")
		result, err := c.API().GetVersionPeople(ctx, &apiclient.GetVersionPeopleRequestOptions{PathParams: &apiclient.GetVersionPeoplePath{VersionID: version}, Query: &apiclient.GetVersionPeopleQuery{Disclosure: &d}})
		return factsResponse{people: result}, err
	})
	if err != nil {
		return nil, err
	}
	if response.noVersion {
		return nil, &searchProfileError{code: "no_content_version", message: "Document has no current content version."}
	}
	if response.dates != nil {
		snapshot, err := validateFactsSnapshot(in, *response.dates)
		if err != nil {
			return nil, err
		}
		return documentDatesPage(*response.dates, in.Offset, snapshot)
	}
	snapshot, err := validateFactsSnapshot(in, *response.people)
	if err != nil {
		return nil, err
	}
	return documentPeoplePage(*response.people, in.Offset, snapshot)
}

func validateFactsSnapshot(in documentFactsInput, value any) (string, error) {
	snapshot, err := factsSnapshot(value)
	if err != nil {
		return "", err
	}
	if in.Offset > 0 && in.Snapshot != snapshot {
		return "", staleFactsPage()
	}
	return snapshot, nil
}

func staleFactsPage() error {
	return &searchProfileError{code: "stale_facts", message: "Document facts changed. Restart at offset zero."}
}

func factsSnapshot(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// documentDatesPage pages claims followed by diagnostics, retaining the primary
// and version metadata on every page. Oversized individual items fail visibly.
func documentDatesPage(dates api.VersionDates, offset int, snapshot string) (documentDatesOutput, error) {
	output := documentDatesOutput{privateCache: newPrivateCache(), VersionDates: dates, Snapshot: snapshot}
	claims := len(dates.Claims)
	err := pageFacts(offset, claims+len(dates.Diagnostics), &output.Offset, &output.NextOffset, func(start, end int) {
		output.Claims = dates.Claims[min(start, claims):min(end, claims)]
		output.Diagnostics = dates.Diagnostics[max(start-claims, 0):max(end-claims, 0)]
	}, func() (int, error) { return toolEnvelopeBytes(output, nil) })
	return output, err
}

// documentPeoplePage traverses edges, assertions and unresolved names in order.
func documentPeoplePage(people api.VersionPeople, offset int, snapshot string) (documentPeopleOutput, error) {
	output := documentPeopleOutput{privateCache: newPrivateCache(), VersionPeople: people, Snapshot: snapshot}
	edges, assertions := len(people.Edges), len(people.Edges)+len(people.Assertions)
	err := pageFacts(offset, assertions+len(people.Unresolved), &output.Offset, &output.NextOffset, func(start, end int) {
		output.Edges = people.Edges[min(start, edges):min(end, edges)]
		output.Assertions = people.Assertions[min(max(start, edges), assertions)-edges : min(max(end, edges), assertions)-edges]
		output.Unresolved = people.Unresolved[max(start-assertions, 0):max(end-assertions, 0)]
	}, func() (int, error) { return toolEnvelopeBytes(output, nil) })
	return output, err
}

// pageFacts bounds both item count and the complete encoded MCP envelope.
func pageFacts(offset, total int, start *int, next **int, window func(int, int), size func() (int, error)) error {
	const budget = maxToolResponseBytes - 8<<10
	*start = min(offset, total)
	window(*start, *start)
	for end := *start + 1; end <= min(total, *start+100); end++ {
		window(*start, end)
		*next = nil
		if end < total {
			*next = new(end)
		}
		n, err := size()
		if err != nil {
			return err
		}
		if n > budget {
			if end == *start+1 {
				return errToolResultTooLarge
			}
			window(*start, end-1)
			*next = new(end - 1)
			break
		}
	}
	return nil
}
