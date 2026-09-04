//go:build cgo

package sqlite_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/sqlite/mattn"
	"go.kenn.io/docbank/sqlite/modernc"
)

func normalizedObservations(observations driverObservations) driverObservations {
	observations.name = ""
	return observations
}

func observationsEqual(left, right driverObservations) bool {
	return reflect.DeepEqual(normalizedObservations(left), normalizedObservations(right))
}

func TestMattnDriverContract(t *testing.T) {
	observations := exerciseDriverContract(t, mattn.Driver{})
	assertDriverContract(t, observations)
}

func TestDriverParity(t *testing.T) {
	var moderncObservations, mattnObservations driverObservations
	t.Run("modernc", func(t *testing.T) {
		moderncObservations = exerciseDriverContract(t, modernc.Driver{})
		assertDriverContract(t, moderncObservations)
	})
	t.Run("mattn", func(t *testing.T) {
		mattnObservations = exerciseDriverContract(t, mattn.Driver{})
		assertDriverContract(t, mattnObservations)
	})

	require.NotEmpty(t, moderncObservations.name)
	require.NotEmpty(t, mattnObservations.name)
	require.NotEqual(t, moderncObservations.name, mattnObservations.name)
	require.True(t, observationsEqual(moderncObservations, mattnObservations))
}
