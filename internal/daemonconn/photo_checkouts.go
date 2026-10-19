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

// checkoutStateFailed names the terminal failed state, factored out because
// the literal otherwise recurs often enough in this package to trip goconst.
const checkoutStateFailed = "failed"

func validatePhotoCheckoutResponse(c store.PhotoCheckout, id string) error {
	if !validUUIDv4(c.ID) || id != "" && c.ID != id || c.ByteLimit < 0 || c.Selection.All && c.ByteLimit == 0 || !slices.Contains([]string{"unbound", "queued", "running", "completed", checkoutStateFailed, "cancelled"}, c.State) {
		return errors.New("invalid photo checkout response identity or state")
	}
	if c.State == "unbound" {
		if c.Destination != "" || c.OperationID != "" {
			return errors.New("unbound photo checkout has local bindings")
		}
	} else {
		if c.OperationID != "" && !validUUIDv4(c.OperationID) || c.LastOperationID != "" && !validUUIDv4(c.LastOperationID) || (c.State == "queued" || c.State == "running") && c.ActiveOperationID() == "" {
			return errors.New("photo checkout response has invalid local bindings")
		}
		// A released, terminal checkout's destination is cleared once its
		// cleared folder is admitted for reuse elsewhere, so an empty
		// destination is only valid in that combination; an active checkout
		// (queued or running) always keeps an absolute destination.
		releasedTerminal := c.RetentionReleased && slices.Contains([]string{"completed", checkoutStateFailed, "cancelled"}, c.State)
		if c.Destination == "" {
			if !releasedTerminal && c.LastOperationKind != store.StorageOperationKindPhotoCheckoutRebind {
				return errors.New("photo checkout response has invalid local bindings")
			}
		} else if !filepath.IsAbs(c.Destination) {
			return errors.New("photo checkout response has invalid local bindings")
		}
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

func (c *Connection) PhotoCheckoutStatus(ctx context.Context, id, after string, limit int) (api.PhotoCheckoutStatus, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.PhotoCheckoutStatus{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	if limit < 1 || limit > 250 || after != "" && !validUUIDv4(after) {
		return api.PhotoCheckoutStatus{}, errors.New("invalid photo checkout status page")
	}
	params := &apiclient.GetPhotoCheckoutQuery{Limit: new(int64(limit))}
	if after != "" {
		value, err := uuid.Parse(after)
		if err != nil {
			return api.PhotoCheckoutStatus{}, fmt.Errorf("parsing checkout cursor: %w", err)
		}
		params.After = &value
		after = value.String()
	}
	out, err := c.API().GetPhotoCheckout(ctx, &apiclient.GetPhotoCheckoutRequestOptions{PathParams: &apiclient.GetPhotoCheckoutPath{ID: parsed}, Query: params})
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
	if len(out.Entries) > limit || int64(len(out.Entries)) > out.Files {
		return api.PhotoCheckoutStatus{}, errors.New("invalid photo checkout status page size")
	}
	previous := after
	for _, entry := range out.Entries {
		if entry.CheckoutID != parsed.String() || !validUUIDv4(entry.FileID) || entry.FileID <= previous || !validUUIDv4(entry.AssetID) || !validUUIDv4(entry.VersionID) || entry.SidecarOfID != nil && !validUUIDv4(*entry.SidecarOfID) {
			return api.PhotoCheckoutStatus{}, errors.New("invalid photo checkout status entry IDs")
		}
		previous = entry.FileID
	}
	if out.NextAfter != "" && (len(out.Entries) != limit || out.NextAfter != previous || !validUUIDv4(out.NextAfter)) {
		return api.PhotoCheckoutStatus{}, errors.New("invalid photo checkout status cursor")
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
	if out.RetiredAt == nil && (!out.RetirementRequested || !slices.Contains([]string{"queued", "running"}, out.LastOperationState)) || out.RetiredAt != nil && !out.RetentionReleased {
		return store.PhotoCheckout{}, errors.New("photo checkout retirement did not release retention")
	}
	return *out, nil
}

func (c *Connection) ScanPhotoCheckout(ctx context.Context, id string) (api.PhotoCheckoutCreateResult, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.PhotoCheckoutCreateResult{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	out, err := c.API().ScanPhotoCheckout(ctx, &apiclient.ScanPhotoCheckoutRequestOptions{PathParams: &apiclient.ScanPhotoCheckoutPath{ID: parsed}})
	return validateCheckoutOperationResponse(out, err, parsed.String(), store.StorageOperationKindPhotoCheckoutScan)
}

func (c *Connection) RebindPhotoCheckout(ctx context.Context, id, destination string) (api.PhotoCheckoutCreateResult, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.PhotoCheckoutCreateResult{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	out, err := c.API().RebindPhotoCheckout(ctx, &apiclient.RebindPhotoCheckoutRequestOptions{PathParams: &apiclient.RebindPhotoCheckoutPath{ID: parsed}, Body: &api.PhotoCheckoutRebindRequest{Destination: destination}})
	return validateCheckoutOperationResponse(out, err, parsed.String(), store.StorageOperationKindPhotoCheckoutRebind)
}

func validateCheckoutOperationResponse(out *api.PhotoCheckoutCreateResult, err error, id, kind string) (api.PhotoCheckoutCreateResult, error) {
	if err != nil {
		return api.PhotoCheckoutCreateResult{}, err
	}
	if out == nil {
		return api.PhotoCheckoutCreateResult{}, errors.New("missing checkout operation")
	}
	operationID := out.Checkout.LastOperationID
	if out.Checkout.LastOperationKind != kind {
		return api.PhotoCheckoutCreateResult{}, errors.New("invalid checkout verification operation")
	}
	if out.Checkout.ID != id || !validUUIDv4(out.Operation.ID) || out.Operation.ID != operationID || out.Operation.Kind != kind || out.Operation.State != out.Checkout.LastOperationState {
		return api.PhotoCheckoutCreateResult{}, errors.New("invalid checkout operation")
	}

	return *out, validatePhotoCheckoutResponse(out.Checkout, id)
}

func (c *Connection) CommitPhotoCheckout(ctx context.Context, id string) (api.PhotoCheckoutCreateResult, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.PhotoCheckoutCreateResult{}, fmt.Errorf("parsing checkout ID: %w", err)
	}
	out, err := c.API().CommitPhotoCheckout(ctx, &apiclient.CommitPhotoCheckoutRequestOptions{PathParams: &apiclient.CommitPhotoCheckoutPath{ID: parsed}})
	return validateCheckoutOperationResponse(out, err, parsed.String(), store.StorageOperationKindPhotoCheckoutCommit)
}
