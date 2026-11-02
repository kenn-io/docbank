package client

import "testing"

func TestPing(t *testing.T) {
	_, _ = Jobs(t.Context(), "http://x/api/v1/jobs")
}
