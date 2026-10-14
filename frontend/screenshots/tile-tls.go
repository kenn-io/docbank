//go:build ignore

package main

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
)

func main() {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	certificate := server.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		panic(err)
	}
	for name, block := range map[string]*pem.Block{
		"ca.pem":   {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		"leaf.pem": {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		"leaf.key": {Type: "PRIVATE KEY", Bytes: key},
	} {
		if err := os.WriteFile(filepath.Join(os.Args[1], name), pem.EncodeToMemory(block), 0o600); err != nil {
			panic(err)
		}
	}
}
