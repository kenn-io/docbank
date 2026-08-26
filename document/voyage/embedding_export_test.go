package voyage

import (
	"crypto/x509"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TrustEmbeddingTestCertificate adds fixture trust to the production transport
// after profile validation; DNS/CIDR and certificate verification still run.
func TrustEmbeddingTestCertificate(t *testing.T, client *EmbeddingClient, certificate *x509.Certificate) {
	t.Helper()
	transport, ok := client.http.Transport.(*http.Transport)
	require.True(t, ok)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(client.http.CloseIdleConnections)
}
