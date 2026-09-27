package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ProductionJobHTTPFixture returns an admitted synthetic job in a real Store.
func ProductionJobHTTPFixture(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	s, request := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), request)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), request.SetID, job.ID
}
