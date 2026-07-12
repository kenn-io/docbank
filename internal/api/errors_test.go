package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestFromMaintenanceErrorPreservesCommittedRetirementBoundary(t *testing.T) {
	err := &packstore.PackRetirementError{PackID: "01kxbz5s5z6b3m8v8m8p0m6h4y", Err: errors.New("sharing violation")}
	mapped := &Error{}
	ok := errors.As(FromMaintenanceError(err), &mapped)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, mapped.Status)
	assert.Equal(t, "pack_retirement_deferred", mapped.Code)
	assert.Contains(t, mapped.Detail, "pack replacement committed")
	assert.Contains(t, mapped.Detail, "docbank storage pack")
}
