package api

import (
	"go.kenn.io/docbank/document"
	"net/http"
)

func registerProcessingConsentRoutes(mux *http.ServeMux, d Deps, g *gate) {
	mux.HandleFunc("POST /api/v1/processing/consents", func(w http.ResponseWriter, r *http.Request) {
		var request document.ProcessingConsentRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		var receipt document.ProcessingConsentReceipt
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.GrantProcessingConsent(r.Context(), request)
			return err
		})
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	mux.HandleFunc("POST /api/v1/processing/consents/revoke", func(w http.ResponseWriter, r *http.Request) {
		var request document.ProcessingConsentRevocationRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		var receipt document.ProcessingConsentRevocationReceipt
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.RevokeProcessingConsent(r.Context(), request)
			return err
		})
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
}
