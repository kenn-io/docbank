package api

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

func TestPassageResolveErrorDoesNotDiscloseAuthorization(t *testing.T) {
	for _, cause := range []error{processing.ErrPassageUnauthorized, processing.ErrPassageUnavailable} {
		mapped := passageResolveError(cause)
		var problem *Error
		require.ErrorAs(t, mapped, &problem)
		assert.Equal(t, http.StatusNotFound, problem.Status)
		assert.Equal(t, "passage_unavailable", problem.Code)
		assert.NotContains(t, problem.Detail, "unauthorized")
	}
	var corrupt *Error
	require.ErrorAs(t, passageResolveError(processing.ErrPassageCorrupt), &corrupt)
	assert.Equal(t, http.StatusInternalServerError, corrupt.Status)
	assert.Equal(t, "passage_corrupt", corrupt.Code)
	var invalid *Error
	require.ErrorAs(t, passageResolveError(processing.ErrPassageInvalid), &invalid)
	assert.Equal(t, http.StatusUnprocessableEntity, invalid.Status)
	assert.Equal(t, "invalid_passage", invalid.Code)
	var internal *Error
	require.ErrorAs(t, passageResolveError(errors.New("synthetic")), &internal)
	assert.Equal(t, "internal", internal.Code)
}

func TestPassageResolutionFromProcessingOwnsSectionPath(t *testing.T) {
	section := []string{"Parent", "Child"}
	locator := &document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
		IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}
	wire := passageResolutionFromProcessing(processing.PassageResolution{
		Availability: "available", Freshness: "historical", PassageID: "passage",
		Text: "exact", SectionPath: section, SourceLocator: locator, SourcePath: "/renamed.md",
	})
	section[0] = "changed"
	locator.Start = 99
	assert.Equal(t, []string{"Parent", "Child"}, wire.SectionPath)
	require.NotNil(t, wire.SourceLocator)
	assert.Equal(t, int64(2), wire.SourceLocator.Start)
	assert.Equal(t, "historical", wire.Freshness)
	assert.Equal(t, "exact", wire.Text)
}

func TestPassageResolutionFromProcessingSerializesEmptySectionPathAsArray(t *testing.T) {
	wire := passageResolutionFromProcessing(processing.PassageResolution{
		Availability: "available", Freshness: "historical", PassageID: "passage", Text: "exact",
	})
	encoded, err := json.Marshal(wire)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"section_path":[]`)
}
