package a

import (
	"net/http"
	"testing"
)

func TestFixtureMuxIsFine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {})
}
