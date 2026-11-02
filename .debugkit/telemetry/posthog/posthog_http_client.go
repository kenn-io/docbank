package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// PostEvent reports through an application's capture route and returns acceptance.
// Call Eligible before each send with the forwarding process's prefix and
// version, because the receiving reporter checks only its own process.
// The caller supplies a nonnil HTTP client and owns its timeouts.
// Callers retry errors on later interaction; the server owns daily deduplication.
func PostEvent(ctx context.Context, client *http.Client, url, event string, properties map[string]any) (Status, error) {
	if client == nil {
		return "", errors.New("send telemetry report: HTTP client is required")
	}
	body, err := json.Marshal(postHogCaptureRequest{Event: event, Properties: properties})
	if err != nil {
		return "", fmt.Errorf("encode telemetry report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create telemetry report: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("send telemetry report: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("telemetry report returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPostHogCaptureBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read telemetry response: %w", err)
	}
	if len(data) > maxPostHogCaptureBodyBytes {
		return "", errors.New("telemetry response too large")
	}
	var result postHogCaptureResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("decode telemetry response: %w", err)
	}
	switch status := Status(result.Status); status {
	case StatusQueued, StatusSkipped, StatusDisabled:
		return status, nil
	default:
		return "", errors.New("unknown telemetry response status")
	}
}
