package api

import (
	"context"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"reflect"
)

func bindProcessingPrincipal(ctx context.Context, catalog *store.Store, supplied string) (string, error) {
	ownerID, bound, noPhotoOwner, err := catalog.PhotoOwnerForRequest(ctx)
	if err != nil {
		return "", err
	}
	if !bound {
		return supplied, nil
	}
	if noPhotoOwner {
		return "", store.ErrNotFound
	}
	if ownerID == "" {
		return supplied, nil
	}
	expected := "owner:" + ownerID
	if supplied != "" && supplied != expected {
		return "", errors.New("processing consent principal does not match the authenticated photo owner")
	}
	return expected, nil
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
		principal, principalErr := bindProcessingPrincipal(r.Context(), d.Store, request.Principal)
		if principalErr != nil {
			writeEmailStoreError(w, principalErr)
			return
		}
		request.Principal = principal
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
		principal, principalErr := bindProcessingPrincipal(r.Context(), d.Store, request.Principal)
		if principalErr != nil {
			writeEmailStoreError(w, principalErr)
			return
		}
		request.Principal = principal
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
