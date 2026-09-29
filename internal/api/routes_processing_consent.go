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

func bindProcessingPrincipal(ctx context.Context, catalog *store.Store, expected, supplied string) (string, error) {
	ownerID, bound, noPhotoOwner, err := catalog.PhotoOwnerForRequest(ctx)
	if err != nil {
		return "", err
	}
	if noPhotoOwner {
		return "", store.ErrNotFound
	}
	if ownerID != "" {
		expected = "owner:" + ownerID
	} else if !bound && expected == "" {
		return "", store.ErrInvalidProcessingConsentRequest
	}
	if supplied != "" && supplied != expected {
		return "", fmt.Errorf("%w: processing consent principal does not match the authenticated photo owner", store.ErrInvalidProcessingConsentRequest)
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
		expected := ""
		if d.Processing != nil {
			var expectedErr error
			expected, expectedErr = d.Processing.PrincipalForRequest(r.Context())
			if expectedErr != nil {
				writeEmailStoreError(w, expectedErr)
				return
			}
		}
		principal, principalErr := bindProcessingPrincipal(r.Context(), d.Store, expected, request.Principal)
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
		expected := ""
		if d.Processing != nil {
			var expectedErr error
			expected, expectedErr = d.Processing.PrincipalForRequest(r.Context())
			if expectedErr != nil {
				writeEmailStoreError(w, expectedErr)
				return
			}
		}
		principal, principalErr := bindProcessingPrincipal(r.Context(), d.Store, expected, request.Principal)
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
