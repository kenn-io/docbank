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
	PersonID string `json:"person_id"`
}

type listPersonCustodiansInput struct {
	PersonID string `json:"person_id"`
	Cursor   string `json:"cursor,omitzero"`
	Limit    int    `json:"limit,omitzero"`
}

func getPerson(ctx context.Context, lease *daemonLease, raw []byte) (personDetailToolOutput, error) {
	var input getPersonInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return personDetailToolOutput{}, err
	}
	detail, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PersonDetail, error) {
		return c.Person(ctx, input.PersonID)
	})
	if err != nil {
		return personDetailToolOutput{}, err
	}
	return personDetailToolOutput{privateCache: newPrivateCache(), PersonDetail: detail}, nil
}

func listPersonCustodians(ctx context.Context, lease *daemonLease, raw []byte) (custodianPageOutput, error) {
	var input listPersonCustodiansInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return custodianPageOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.CustodianPage, error) {
		return c.PersonCustodians(ctx, input.PersonID, input.Cursor, input.Limit)
	})
	if err != nil {
		return custodianPageOutput{}, err
	}
	return custodianPageOutput{CustodianPage: page, privateCache: newPrivateCache()}, nil
}
