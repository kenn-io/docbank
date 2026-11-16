package daemonconn

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// PrepareOriginalDownload issues a capability without copying the original bytes.
// It resolves the returned relative path on the pinned daemon.
func (c *Connection) PrepareOriginalDownload(ctx context.Context, versionID string, selection api.OriginalDownloadSelection) (*api.OriginalDownloadTicket, error) {
	version, err := uuid.Parse(versionID)
	if err != nil {
		return nil, errors.New("original download version is invalid")
	}
	ticket, err := c.API().PrepareOriginalDownload(ctx, &apiclient.PrepareOriginalDownloadRequestOptions{
		PathParams: &apiclient.PrepareOriginalDownloadPath{VersionID: version}, Body: &selection,
	})
	if err != nil {
		return nil, err
	}
	ticket.URL = c.base + ticket.URL
	return ticket, nil
}

// RoundTripOriginalDownload builds and spends a capability on this connection.
// It never follows redirects, adds a key, or forwards headers.
func (c *Connection) RoundTripOriginalDownload(ctx context.Context, ticket string) (*http.Response, error) {
	address := c.base + "/api/daemon/web-download/file?" + url.Values{"ticket": {ticket}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("original download request is invalid")
	}
	transport := c.hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(request)
}
