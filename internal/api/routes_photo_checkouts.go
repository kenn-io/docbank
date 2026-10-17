package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/checkout"
	"go.kenn.io/docbank/internal/store"
)

type PhotoCheckoutCreateRequest struct {
	Selection   store.PhotoCheckoutSelection `json:"selection"`
	Destination string                       `json:"destination" minLength:"1" maxLength:"4096"`
	ByteLimit   int64                        `json:"byte_limit" minimum:"0"`
}

type PhotoCheckoutCreateResult struct {
	Checkout  store.PhotoCheckout `json:"checkout"`
	Operation StorageOperation    `json:"operation"`
}

type PhotoCheckoutList struct {
	Items []store.PhotoCheckout `json:"items"`
}

type PhotoCheckoutStatus = store.PhotoCheckoutStatus

type PhotoCheckoutRebindRequest struct {
	Destination string `json:"destination" minLength:"1" maxLength:"4096"`
}

func NewPhotoCheckoutService(d Deps) checkout.Service {
	s := checkout.Service{Store: d.Store, Blobs: d.Blobs, VaultRoot: d.VaultRoot, Config: d.Cfg}
	if d.Gate != nil {
		s.Mutate = d.Gate.MutateContext
	}
	if d.Tracker != nil {
		s.ActivityBegin = d.Tracker.Begin
		s.ActivityEnd = d.Tracker.End
	}
	return s
}

func registerPhotoCheckoutRoutes(api huma.API, d Deps, g *gate) {
	d.Gate = g
	service := NewPhotoCheckoutService(d)
	huma.Register(api, huma.Operation{OperationID: "estimatePhotoCheckout", Method: http.MethodPost, Path: "/api/v1/photos/checkouts/estimate", Summary: "Estimate writable photo copies"}, func(ctx context.Context, in *struct{ Body store.PhotoCheckoutSelection }) (*struct{ Body store.PhotoCheckoutEstimate }, error) {
		out, err := d.Store.EstimatePhotoCheckout(ctx, in.Body)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body store.PhotoCheckoutEstimate }{out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createPhotoCheckout", Method: http.MethodPost, Path: "/api/v1/photos/checkouts", Summary: "Create verified writable photo copies in an empty daemon-host folder", DefaultStatus: http.StatusAccepted}, func(ctx context.Context, in *struct{ Body PhotoCheckoutCreateRequest }) (*struct{ Body PhotoCheckoutCreateResult }, error) {
		if d.Jobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "photo_checkout_unavailable", "background job supervisor is unavailable")
		}
		c, op, _, root, err := service.Prepare(ctx, in.Body.Selection, in.Body.Destination, in.Body.ByteLimit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if err := service.Start(d.Jobs, c, root); err != nil {
			d.Logger.Error("start queued photo checkout", "operation_id", op.ID, "error", err)
			return nil, FromStoreError(err)
		}
		return &struct{ Body PhotoCheckoutCreateResult }{PhotoCheckoutCreateResult{Checkout: c, Operation: storageOperationAPI(op)}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "listPhotoCheckouts", Method: http.MethodGet, Path: "/api/v1/photos/checkouts", Summary: "List photo checkouts"}, func(ctx context.Context, _ *struct{}) (*struct{ Body PhotoCheckoutList }, error) {
		items, err := d.Store.ListPhotoCheckouts(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body PhotoCheckoutList }{PhotoCheckoutList{Items: items}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getPhotoCheckout", Method: http.MethodGet, Path: "/api/v1/photos/checkouts/{id}", Summary: "Inspect photo checkout progress"}, func(ctx context.Context, in *struct {
		ID    string `path:"id" format:"uuid"`
		After string `query:"after" format:"uuid"`
		Limit int    `query:"limit" default:"100" minimum:"1" maximum:"250"`
	}) (*struct{ Body PhotoCheckoutStatus }, error) {
		status, err := d.Store.PhotoCheckoutStatus(ctx, in.ID, in.After, in.Limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body PhotoCheckoutStatus }{status}, nil
	})
	start := func(ctx context.Context, id, kind, destination string) (*struct{ Body PhotoCheckoutCreateResult }, error) {
		if d.Jobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "photo_checkout_unavailable", "background job supervisor is unavailable")
		}
		c, op, err := service.PrepareOperation(ctx, id, kind, destination)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if err := service.StartOperation(d.Jobs, c); err != nil {
			message := "start checkout verification"
			if kind == store.StorageOperationKindPhotoCheckoutCommit {
				message = "start queued checkout commit"
			}
			d.Logger.Error(message, "operation_id", op.ID, "error", err)
			return nil, FromStoreError(err)
		}
		return &struct{ Body PhotoCheckoutCreateResult }{PhotoCheckoutCreateResult{Checkout: c, Operation: storageOperationAPI(op)}}, nil
	}
	for _, rebind := range []bool{false, true} {
		action, summary := "scan", "Inspect checkout edits"
		kind := store.StorageOperationKindPhotoCheckoutScan
		if rebind {
			action, summary = "rebind", "Verify a checkout folder"
			kind = store.StorageOperationKindPhotoCheckoutRebind
		}
		operation := huma.Operation{OperationID: action + "PhotoCheckout", Method: http.MethodPost, Path: "/api/v1/photos/checkouts/{id}/" + action, Summary: summary, DefaultStatus: http.StatusAccepted}
		if rebind {
			huma.Register(api, operation, func(ctx context.Context, in *struct {
				ID   string `path:"id" format:"uuid"`
				Body PhotoCheckoutRebindRequest
			}) (*struct{ Body PhotoCheckoutCreateResult }, error) {
				return start(ctx, in.ID, kind, in.Body.Destination)
			})
		} else {
			huma.Register(api, operation, func(ctx context.Context, in *struct {
				ID string `path:"id" format:"uuid"`
			}) (*struct{ Body PhotoCheckoutCreateResult }, error) {
				return start(ctx, in.ID, kind, "")
			})
		}
	}
	huma.Register(api, huma.Operation{OperationID: "commitPhotoCheckout", Method: http.MethodPost, Path: "/api/v1/photos/checkouts/{id}/commit", Summary: "Commit settled checkout edits as new versions", DefaultStatus: http.StatusAccepted}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{ Body PhotoCheckoutCreateResult }, error) {
		return start(ctx, in.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	})
	huma.Register(api, huma.Operation{OperationID: "retirePhotoCheckout", Method: http.MethodPost, Path: "/api/v1/photos/checkouts/{id}/retire", Summary: "Release photo checkout holds and destination"}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{ Body store.PhotoCheckout }, error) {
		var out store.PhotoCheckout
		err := g.MutateContext(ctx, func() error { var err error; out, err = d.Store.RetirePhotoCheckout(ctx, in.ID); return err })
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body store.PhotoCheckout }{out}, nil
	})
}
