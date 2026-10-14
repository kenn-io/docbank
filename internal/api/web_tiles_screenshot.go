//go:build screenshot_tiles

package api

import (
	"crypto/x509"
	"errors"
	"os"
)

// Screenshot builds trust only the synthetic provider certificate.
func init() {
	mapTileRootCAs = func() (*x509.CertPool, error) {
		raw, err := os.ReadFile(os.Getenv("DOCBANK_MAP_TILE_TEST_CA"))
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(raw) {
			return nil, errors.New("DOCBANK_MAP_TILE_TEST_CA holds no certificate")
		}
		return roots, nil
	}
}
