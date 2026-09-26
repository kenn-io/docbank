package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
)

// ExportArchiveAuthority reads the current owner-bound receipt after the
// daemon checks the plan and every live source member. It transfers no bytes.
func (c *Connection) ExportArchiveAuthority(ctx context.Context, jobID string) (bundle.Receipt, error) {
	if !IsCanonicalUUIDv4(jobID) {
		return bundle.Receipt{}, errors.New("invalid export job ID")
	}
	receipt, err := c.API().GetExportArchiveAuthority(ctx, &apiclient.GetExportArchiveAuthorityRequestOptions{
		PathParams: &apiclient.GetExportArchiveAuthorityPath{ID: jobID},
	})
	if err != nil {
		return bundle.Receipt{}, err
	}
	if receipt == nil || receipt.Format != bundle.Format || receipt.Size < 1 ||
		receipt.Size > bundle.MaxArchiveBytes || receipt.Entries < 1 ||
		!canonical.IsSHA256Hex(receipt.SHA256) || !canonical.IsSHA256Hex(receipt.PlanFingerprint) {
		return bundle.Receipt{}, errors.New("export archive authority lacks a bounded retained receipt")
	}
	return *receipt, nil
}
