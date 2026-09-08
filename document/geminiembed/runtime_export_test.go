package geminiembed

import "net/http"

// SetEmbeddingTestTransport substitutes only the HTTP exchange for runtime
// integration tests; profile, proof, request, and response validation remain
// production code paths.
func SetEmbeddingTestTransport(client *Client, transport http.RoundTripper) {
	client.http.Transport = transport
}
