package api

import (
	"context"
	"fmt"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"reflect"
)

// checkProcessingPrincipal refuses a consent principal other than the photo
// owner a request selected by header or browser session.
func checkProcessingPrincipal(ctx context.Context, principal string) error {
	if ownerID, _ := store.PhotoOwnerFromContext(ctx); ownerID != "" && principal != "owner:"+ownerID {
		return fmt.Errorf("%w: principal must be the authenticated photo owner", store.ErrInvalidProcessingConsentRequest)
	}
	return nil
}

func registerProcessingConsentRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "grantProcessingConsent", Method: http.MethodPost,
		Path: "/api/v1/processing/consents", Summary: "Explicitly grant existing processing consent",
	}, reflect.TypeFor[document.ProcessingConsentRequest](), reflect.TypeFor[document.ProcessingConsentReceipt](), func(w http.ResponseWriter, r *http.Request) {
		var request document.ProcessingConsentRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		if err := checkProcessingPrincipal(r.Context(), request.Principal); err != nil {
			writeEmailStoreError(w, err)
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
	registerEmailDocumentRoute(mux, api, huma.Operation{
		OperationID: "revokeProcessingConsent", Method: http.MethodPost,
		Path: "/api/v1/processing/consents/revoke", Summary: "Revoke a principal and scope before further provider access",
	}, reflect.TypeFor[document.ProcessingConsentRevocationRequest](), reflect.TypeFor[document.ProcessingConsentRevocationReceipt](), func(w http.ResponseWriter, r *http.Request) {
		var request document.ProcessingConsentRevocationRequest
		if !readEmailDocumentJSON(w, r, &request) {
			return
		}
		if err := checkProcessingPrincipal(r.Context(), request.Principal); err != nil {
			writeEmailStoreError(w, err)
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
