package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/embedlocal"
)

// Kit probes model inventory before reusing a running server. A ping response
// on this path is not a model inventory, even when its HTTP status is 200.
func withTextModelInventory(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, map[string][]string{"models": {embedlocal.TextModel}}))
			return
		}
		next.ServeHTTP(w, r)
	})
}
