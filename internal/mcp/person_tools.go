package mcp

import (
	"context"
	"encoding/json/v2"

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
	const pageBudget = maxToolResponseBytes/3 - 8<<10
	for {
		encoded, err := json.Marshal(output)
		if err != nil {
			return personDetailToolOutput{}, err
		}
		if len(encoded) <= pageBudget {
			return output, nil
		}
		if len(output.Assertions) <= 1 {
			return personDetailToolOutput{}, errToolResultTooLarge
		}
		end := max(1, len(output.Assertions)*pageBudget/len(encoded))
		output.Assertions = output.Assertions[:end]
		output.AssertionsTruncated = true
		output.AssertionsNextCursor = output.Assertions[len(output.Assertions)-1].AssertionID
	}
}
