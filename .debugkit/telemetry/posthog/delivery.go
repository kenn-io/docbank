package posthog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"

	phsdk "github.com/posthog/posthog-go"
)

// Delivery identifies an event across retries; persist it before sending.
type Delivery struct {
	ID   uuid.UUID
	Time time.Time
}

// CaptureConfirmed waits for PostHog API acceptance of a non-daily event.
// Only (StatusDelivered, nil) confirms completion; (StatusDisabled, nil)
// leaves it pending. Reuse Delivery, event, and DistinctID after errors or
// cancellation, since delivery may remain in flight. A deadline bounds the
// wait, not the SDK's retries. Flushing also sends other queued events.
func (r *Reporter) CaptureConfirmed(ctx context.Context, event string, properties map[string]any, delivery Delivery) (Status, error) {
	if delivery.ID == uuid.Nil() || delivery.Time.IsZero() {
		return "", errors.New("telemetry delivery requires a nonzero UUID and timestamp")
	}
	event = strings.TrimSpace(event)
	props, err := r.SanitizeProperties(event, properties)
	if err != nil {
		return "", err
	}
	if _, daily := r.dailyEvents[event]; daily {
		return "", fmt.Errorf("%w: confirmed delivery of daily event %q", ErrUnsupportedEvent, event)
	}
	if !r.Enabled() {
		return StatusDisabled, nil
	}
	if err := checkEncodable(props); err != nil {
		return "", err
	}
	key := deliveryKey{delivery.ID.String(), event, r.distinctID, delivery.Time.UTC()}
	result := make(chan error, 1)
	r.deliveries.mu.Lock()
	r.deliveries.pending[result] = key
	r.deliveries.mu.Unlock()
	defer func() {
		r.deliveries.mu.Lock()
		delete(r.deliveries.pending, result)
		r.deliveries.mu.Unlock()
	}()
	status, client, err := r.queue(ctx, event, props, delivery.Time, key.id)
	if err != nil || client == nil {
		return status, err
	}
	// Close resolves every pending delivery after its drain, so a flush
	// refused by a concurrent Close still waits for the drained result.
	if flushErr := client.FlushWithContext(ctx); flushErr != nil && !errors.Is(flushErr, phsdk.ErrClosed) {
		select {
		case err = <-result:
		default:
			return "", flushErr
		}
	} else {
		select {
		case err = <-result:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if ProcessDisabled() {
		return StatusDisabled, nil
	}
	if err != nil {
		return "", err
	}
	return StatusDelivered, nil
}

type deliveryKey struct {
	id, event, distinctID string
	timestamp             time.Time
}
type deliveryCallbacks struct {
	mu      sync.Mutex
	pending map[chan error]deliveryKey
}

func (d *deliveryCallbacks) Success(message phsdk.APIMessage) { d.notify(message, nil) }
func (d *deliveryCallbacks) Failure(message phsdk.APIMessage, err error) {
	d.notify(message, err)
}

func (d *deliveryCallbacks) notify(message phsdk.APIMessage, err error) {
	capture, ok := message.(phsdk.CaptureInApi)
	if !ok {
		return
	}
	key := deliveryKey{capture.Uuid, capture.Event, capture.DistinctId, capture.Timestamp.UTC()}
	d.resolve(func(pending deliveryKey) bool { return pending == key }, err)
}

// closeAll fails every pending delivery once the client stops.
func (d *deliveryCallbacks) closeAll(err error) {
	d.resolve(func(deliveryKey) bool { return true }, err)
}

func (d *deliveryCallbacks) resolve(match func(deliveryKey) bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for result, pending := range d.pending {
		if match(pending) {
			result <- err
			delete(d.pending, result)
		}
	}
}
