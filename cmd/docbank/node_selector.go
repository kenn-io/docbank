package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

const nodeIDSelectorPrefix = "id:"

type nodeSelector struct {
	raw  string
	path string
	id   int64
}

func parseNodeSelector(raw string) (nodeSelector, error) {
	selector := nodeSelector{raw: raw}
	if after, ok := strings.CutPrefix(raw, nodeIDSelectorPrefix); ok {
		digits := after
		id, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != digits {
			return nodeSelector{}, usageError(fmt.Errorf(
				"invalid node selector %q: use id:<positive-decimal>", raw))
		}
		selector.id = id
		return selector, nil
	}
	if !strings.HasPrefix(raw, "/") {
		return nodeSelector{}, usageError(errors.New(
			"node selector must be an absolute virtual path or id:<positive-decimal>"))
	}
	selector.path = raw
	return selector, nil
}

func (s nodeSelector) resolve(ctx context.Context, c *client.Client) (api.Node, error) {
	n, err := s.resolveIncludingTrash(ctx, c)
	if err != nil {
		return api.Node{}, err
	}
	if n.TrashedAt != "" {
		return api.Node{}, fmt.Errorf("resolving %q: node is trashed: %w", s.raw, store.ErrNotFound)
	}
	return n, nil
}

func (s nodeSelector) resolveIncludingTrash(
	ctx context.Context, c *client.Client,
) (api.Node, error) {
	var (
		n   api.Node
		err error
	)
	if s.id != 0 {
		n, err = c.Node(ctx, s.id)
	} else {
		n, err = c.Stat(ctx, s.path)
	}
	if err != nil {
		return api.Node{}, fmt.Errorf("resolving %q: %w", s.raw, err)
	}
	return n, nil
}

func (s nodeSelector) isID() bool { return s.id != 0 }

func formatNodeSelector(id int64) string {
	return nodeIDSelectorPrefix + strconv.FormatInt(id, 10)
}

func parseRestoreNodeID(raw string) (int64, error) {
	if strings.HasPrefix(raw, nodeIDSelectorPrefix) {
		selector, err := parseNodeSelector(raw)
		if err != nil {
			return 0, err
		}
		return selector.id, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, usageError(fmt.Errorf(
			"invalid node id %q: use a positive decimal or id:<positive-decimal>", raw))
	}
	return id, nil
}
