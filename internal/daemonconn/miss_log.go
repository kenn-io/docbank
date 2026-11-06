package daemonconn

import (
	"context"
	"time"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/misslog"
)

// RecordMiss sends a best-effort receipt within a one-second deadline.
func (c *Connection) RecordMiss(ctx context.Context, receipt misslog.Request) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _ = c.API().RecordMiss(ctx, &apiclient.RecordMissRequestOptions{Body: &receipt})
}
