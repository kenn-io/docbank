package daemonconn

import (
	"net/http"

	"context"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
)

func (c *Connection) GrantScopedProcessingConsent(ctx context.Context, r document.ProcessingConsentRequest) (document.ProcessingConsentReceipt, error) {
	var out document.ProcessingConsentReceipt
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).GrantProcessingConsent(runtime.WithStreamingResponse(ctx), &apiclient.GrantProcessingConsentRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	return out, err
}
func (c *Connection) RevokeScopedProcessingConsent(ctx context.Context, r document.ProcessingConsentRevocationRequest) (document.ProcessingConsentRevocationReceipt, error) {
	var out document.ProcessingConsentRevocationReceipt
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).RevokeProcessingConsent(runtime.WithStreamingResponse(ctx), &apiclient.RevokeProcessingConsentRequestOptions{Body: &r}, limitEmailDocumentRequest)
	if err == nil {
		err = decodeEmailDocuments(responseHTTP, &out)
	}
	return out, err
}
