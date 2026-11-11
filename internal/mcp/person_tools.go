package mcp

import (
	"context"
	"encoding/json/v2"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type personDetailToolOutput struct {
	privateCache
	api.PersonDetail
}

type getPersonInput struct {
	PersonID        string `json:"person_id"`
	AssertionCursor string `json:"assertion_cursor"`
	AssertionLimit  int    `json:"assertion_limit"`
}

func getPerson(ctx context.Context, lease *daemonLease, raw []byte) (personDetailToolOutput, error) {
	var input getPersonInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return personDetailToolOutput{}, err
	}
	detail, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PersonDetail, error) {
		return c.PersonPage(ctx, input.PersonID, api.PersonAssertionPage{Cursor: input.AssertionCursor, Limit: input.AssertionLimit})
	})
	if err != nil {
		return personDetailToolOutput{}, err
	}
	output := personDetailToolOutput{privateCache: newPrivateCache(), PersonDetail: detail}
	err = fitToolPage(&output, func() int { return len(output.Assertions) }, func(end int) {
		output.Assertions = output.Assertions[:end]
		output.AssertionsTruncated = true
		output.AssertionsNextCursor = output.Assertions[end-1].AssertionID
	})
	return output, err
}

func fitToolPage(output any, count func() int, shrink func(int)) error {
	// The real ceiling is the complete MCP envelope boundedToolSuccess
	// builds for every read tool: the structured content plus its duplicate
	// escaped-text copy. A fraction of the raw struct rejects identities
	// that cannot be paginated even when the actual response still fits.
	const envelopeBudget = maxToolResponseBytes - 8<<10
	for {
		envelope, err := toolEnvelopeBytes(output, nil)
		if err != nil {
			return err
		}
		if envelope <= envelopeBudget {
			return nil
		}
		if count() <= 1 {
			return errToolResultTooLarge
		}
		end := max(1, min(count()-1, count()*envelopeBudget/envelope))
		shrink(end)
	}
}

// toolEnvelopeBytes measures the complete MCP result a read response
// produces once wrapped, matching the shape boundedToolSuccess builds: the
// structured content plus its duplicate escaped-text copy and resource links.
// Paged tools check this before giving up on pagination, instead of a heuristic
// fraction of the raw struct.
func toolEnvelopeBytes(output any, links []*sdkmcp.ResourceLink) (int, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return 0, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return 0, err
	}
	result := toolResult(encoded, normalized, links)
	complete, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	return len(complete), nil
}
