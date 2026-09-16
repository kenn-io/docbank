package daemonconn

import (
	"context"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
)

func (c *Connection) GrantScopedProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	var out document.ProcessingConsentReceipt
	response, err := c.API().GrantProcessingConsentWithResponse(runtime.WithStreamingResponse(ctx), &apiclient.GrantProcessingConsentRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(response.HTTPResponse, &out)
	}
	return out, err
}
func (c *Connection) RevokeScopedProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	var out document.ProcessingConsentRevocationReceipt
	response, err := c.API().RevokeProcessingConsentWithResponse(runtime.WithStreamingResponse(ctx), &apiclient.RevokeProcessingConsentRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(response.HTTPResponse, &out)
	}
	return out, err
}
