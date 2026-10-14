package mcp

import (
	"context"

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
		return c.Person(ctx, input.PersonID, api.PersonAssertionPage{Cursor: input.AssertionCursor, Limit: input.AssertionLimit})
	})
	if err != nil {
		return personDetailToolOutput{}, err
	}
	return personDetailToolOutput{privateCache: newPrivateCache(), PersonDetail: detail}, nil
}
