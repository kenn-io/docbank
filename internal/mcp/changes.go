package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const maxChangeCursorBytes = 128
const maxChangesPage = 500

type changesInput struct {
	Cursor string `json:"cursor"`
}

type changesOutput struct {
	privateCache

	Events     []store.VaultChange `json:"events"`
	NextCursor string              `json:"next_cursor"`
	HasMore    bool                `json:"has_more"`
}

func listChanges(ctx context.Context, lease *daemonLease, raw []byte) (changesOutput, error) {
	var input changesInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return changesOutput{}, err
	}
	limit := int64(maxChangesPage)
	if input.Cursor == "" {
		limit = 0
	}
	return daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (changesOutput, error) {
		page := changesOutput{Events: make([]store.VaultChange, 0), privateCache: newPrivateCache()}
		head, err := c.WatchChanges(ctx, input.Cursor, false, &limit, func(event store.VaultChange) error {
			page.Events = append(page.Events, event)
			page.NextCursor = event.Cursor
			return nil
		})
		if err != nil {
			return changesOutput{}, err
		}
		if len(page.Events) == 0 {
			page.NextCursor = head
		}
		page.HasMore = page.NextCursor != head
		return page, nil
	})
}
