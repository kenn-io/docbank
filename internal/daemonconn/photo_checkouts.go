package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

func validatePhotoCheckoutResponse(c store.PhotoCheckout, id string) error {
	if !validUUIDv4(c.ID) || id != "" && c.ID != id || c.ByteLimit < 0 || c.Selection.All && c.ByteLimit == 0 || !slices.Contains([]string{"unbound", "queued", "running", "completed", "failed", "cancelled"}, c.State) {
		return errors.New("invalid photo checkout response identity or state")
	}
	if c.State == "unbound" {
		if c.Destination != "" || c.OperationID != "" {
			return errors.New("unbound photo checkout has local bindings")
		}
	} else if !filepath.IsAbs(c.Destination) || !validUUIDv4(c.OperationID) {
		return errors.New("photo checkout response has invalid local bindings")
	}
	return store.ValidatePhotoCheckoutSelection(c.Selection)
}

func (c *Connection) EstimatePhotoCheckout(ctx context.Context, selection store.PhotoCheckoutSelection) (store.PhotoCheckoutEstimate, error) {
	out, err := c.API().EstimatePhotoCheckout(ctx, &apiclient.EstimatePhotoCheckoutRequestOptions{Body: &selection})
	if err != nil {
		return store.PhotoCheckoutEstimate{}, err
	}
	if out == nil || out.Assets < 0 || out.Files < 0 || out.Bytes < 0 {
		return store.PhotoCheckoutEstimate{}, errors.New("invalid photo checkout estimate response")
	}
	return *out, nil
}

func (c *Connection) CreatePhotoCheckout(ctx context.Context, request api.PhotoCheckoutCreateRequest) (api.PhotoCheckoutCreateResult, error) {
	out, err := c.API().CreatePhotoCheckout(ctx, &apiclient.CreatePhotoCheckoutRequestOptions{Body: &request})
	if err != nil {
		return api.PhotoCheckoutCreateResult{}, err
	}
	if out == nil {
		return api.PhotoCheckoutCreateResult{}, &responseDecodeError{err: errors.New("missing photo checkout creation response")}
	}
	if err := validatePhotoCheckoutResponse(out.Checkout, ""); err != nil {
		return api.PhotoCheckoutCreateResult{}, &responseDecodeError{err: err}
	}
	if out.Operation.ID != out.Checkout.OperationID || out.Operation.Kind != store.StorageOperationKindPhotoCheckout || out.Operation.State != out.Checkout.State || !slices.Contains([]string{"queued", "running", "completed", "failed", "cancelled"}, out.Operation.State) {
		return api.PhotoCheckoutCreateResult{}, &responseDecodeError{err: errors.New("invalid photo checkout creation operation")}
	}
	if slices.Contains([]string{"completed", "failed", "cancelled"}, out.Operation.State) && out.Operation.FinishedAt == "" {
		return api.PhotoCheckoutCreateResult{}, &responseDecodeError{err: errors.New("terminal photo checkout creation has no finish time")}
	}
	if out.Operation.State == "failed" && (out.Operation.Error == "" || out.Checkout.Error != out.Operation.Error) {
		return api.PhotoCheckoutCreateResult{}, &responseDecodeError{err: errors.New("failed photo checkout creation has no terminal error")}
	}
	return *out, nil
}

func (c *Connection) PhotoCheckouts(ctx context.Context) ([]store.PhotoCheckout, error) {
	out, err := c.API().ListPhotoCheckouts(ctx)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("missing photo checkouts response")
	}
	for _, v := range out.Items {
		if err := validatePhotoCheckoutResponse(v, ""); err != nil {
			return nil, err
		}
	}
	return out.Items, nil
}

func (c *Connection) PhotoCheckoutStatus(ctx context.Context, id string) (api.PhotoCheckoutStatus, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.PhotoCheckoutStatus{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	out, err := c.API().GetPhotoCheckout(ctx, &apiclient.GetPhotoCheckoutRequestOptions{PathParams: &apiclient.GetPhotoCheckoutPath{ID: parsed}})
	if err != nil {
		return api.PhotoCheckoutStatus{}, err
	}
	if out == nil {
		return api.PhotoCheckoutStatus{}, errors.New("missing photo checkout status response")
	}
	if err := validatePhotoCheckoutResponse(out.Checkout, parsed.String()); err != nil {
		return api.PhotoCheckoutStatus{}, err
	}
	if out.Files < 0 || out.Bytes < 0 || out.PublishedFiles < 0 || out.PublishedFiles > out.Files || out.PublishedBytes < 0 || out.PublishedBytes > out.Bytes {
		return api.PhotoCheckoutStatus{}, errors.New("invalid photo checkout status counts")
	}
	return *out, nil
}

func (c *Connection) RetirePhotoCheckout(ctx context.Context, id string) (store.PhotoCheckout, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return store.PhotoCheckout{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	out, err := c.API().RetirePhotoCheckout(ctx, &apiclient.RetirePhotoCheckoutRequestOptions{PathParams: &apiclient.RetirePhotoCheckoutPath{ID: parsed}})
	if err != nil {
		return store.PhotoCheckout{}, err
	}
	if out == nil {
		return store.PhotoCheckout{}, errors.New("missing photo checkout retirement response")
	}
	if err := validatePhotoCheckoutResponse(*out, parsed.String()); err != nil {
		return store.PhotoCheckout{}, err
	}
	if out.RetiredAt == nil || !out.RetentionReleased {
		return store.PhotoCheckout{}, errors.New("photo checkout retirement did not release retention")
	}
	return *out, nil
}
