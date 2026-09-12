package client

import (
	"context"
	"go.kenn.io/docbank/document"
	"net/http"
)

func (c *Client) GrantProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	var out document.ProcessingConsentReceipt
	err := c.doEmailDocuments(ctx, http.MethodPost, "/api/v1/processing/consents", r, &out)
	return out, err
}
func (c *Client) RevokeProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	var out document.ProcessingConsentRevocationReceipt
	err := c.doEmailDocuments(ctx, http.MethodPost, "/api/v1/processing/consents/revoke", r, &out)
	return out, err
}
