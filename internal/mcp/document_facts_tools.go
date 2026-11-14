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
		if err := validateFactsSnapshot(in, *response.dates); err != nil {
			return nil, err
		}
		return documentDatesPage(*response.dates, in.Offset)
	}
	if err := validateFactsSnapshot(in, *response.people); err != nil {
		return nil, err
	}
	return documentPeoplePage(*response.people, in.Offset)
}

func validateFactsSnapshot(in documentFactsInput, value any) error {
	if in.Offset == 0 {
		return nil
	}
	snapshot, err := factsSnapshot(value)
	if err != nil {
		return err
	}
	if in.Snapshot != snapshot {
		return staleFactsPage()
	}
	return nil
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
func documentDatesPage(dates api.VersionDates, offset int) (documentDatesOutput, error) {
	total := len(dates.Claims) + len(dates.Diagnostics)
	start := min(offset, total)
	output := documentDatesOutput{privateCache: newPrivateCache(), VersionDates: dates, Offset: start}
	snapshot, err := factsSnapshot(dates)
	if err != nil {
		return output, err
	}
	output.Snapshot = snapshot
	output.Claims = []api.VersionDateClaim{}
	output.Diagnostics = []api.FactDiagnostic{}
	err = pageFacts(start, total, &output.NextOffset, func(i int, add bool) {
		if i < len(dates.Claims) {
			if add {
				output.Claims = append(output.Claims, dates.Claims[i])
			} else {
				output.Claims = output.Claims[:len(output.Claims)-1]
			}
		} else {
			if add {
				output.Diagnostics = append(output.Diagnostics, dates.Diagnostics[i-len(dates.Claims)])
			} else {
				output.Diagnostics = output.Diagnostics[:len(output.Diagnostics)-1]
			}
		}
	}, func() (int, error) { return toolEnvelopeBytes(output, nil) })
	return output, err
}

// documentPeoplePage traverses edges, assertions and unresolved names in order.
func documentPeoplePage(people api.VersionPeople, offset int) (documentPeopleOutput, error) {
	edges, assertions := len(people.Edges), len(people.Assertions)
	total := edges + assertions + len(people.Unresolved)
	start := min(offset, total)
	output := documentPeopleOutput{privateCache: newPrivateCache(), VersionPeople: people, Offset: start}
	snapshot, err := factsSnapshot(people)
	if err != nil {
		return output, err
	}
	output.Snapshot = snapshot
	output.Edges = []api.VersionPersonEdge{}
	output.Assertions = []api.VersionPersonAssertion{}
	output.Unresolved = []api.VersionUnresolvedName{}
	err = pageFacts(start, total, &output.NextOffset, func(i int, add bool) {
		switch {
		case i < edges:
			if add {
				output.Edges = append(output.Edges, people.Edges[i])
			} else {
				output.Edges = output.Edges[:len(output.Edges)-1]
			}
		case i < edges+assertions:
			if add {
				output.Assertions = append(output.Assertions, people.Assertions[i-edges])
			} else {
				output.Assertions = output.Assertions[:len(output.Assertions)-1]
			}
		default:
			if add {
				output.Unresolved = append(output.Unresolved, people.Unresolved[i-edges-assertions])
			} else {
				output.Unresolved = output.Unresolved[:len(output.Unresolved)-1]
			}
		}
	}, func() (int, error) { return toolEnvelopeBytes(output, nil) })
	return output, err
}

// pageFacts bounds both item count and the complete encoded MCP envelope.
func pageFacts(start, total int, next **int, item func(int, bool), size func() (int, error)) error {
	const budget = maxToolResponseBytes - 8<<10
	for i := start; i < min(total, start+100); i++ {
		item(i, true)
		if i+1 < total {
			*next = new(i + 1)
		} else {
			*next = nil
		}
		n, err := size()
		if err != nil {
			return err
		}
		if n > budget {
			if i == start {
				return errToolResultTooLarge
			}
			item(i, false)
			*next = new(i)
			break
		}
	}
	return nil
}
