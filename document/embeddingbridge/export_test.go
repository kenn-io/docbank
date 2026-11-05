package embeddingbridge

import (
	"context"
	"net"
	"net/http"
)

// SetTestDialer replaces how client's sealed transport opens connections, so a
// test can serve the bridge over in-memory connections inside a synctest
// bubble. Request validation and every other transport setting still apply.
func SetTestDialer(client *Client, dial func(context.Context, string, string) (net.Conn, error)) {
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		panic("embedding bridge transport is not an *http.Transport")
	}
	transport = transport.Clone()
	transport.DialContext = dial
	client.http.Transport = transport
}
