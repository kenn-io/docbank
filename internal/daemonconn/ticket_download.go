package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// openTicketDownload uses the daemon connection without exposing the ticket in
// errors. The caller owns the response and validates its format-specific bytes.
func (c *Connection) openTicketDownload(
	ctx context.Context, ticket string,
) (*http.Response, error) {
	u, err := url.Parse(ticket)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
		u.Fragment != "" || u.Path != "/api/daemon/web-download/file" ||
		u.Query().Get("ticket") == "" {
		return nil, integrityErrorf("export ticket URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+ticket, nil)
	if err != nil {
		return nil, errors.New("cannot build export download request")
	}
	request.Header.Set("X-Api-Key", c.key)
	// #nosec G704 -- Validated relative ticket path on the existing daemon connection.
	response, err := c.hc.Do(request)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err // The URL contains a one-use download credential.
		}
		return nil, fmt.Errorf("downloading export: %w", err)
	}
	return response, nil
}
