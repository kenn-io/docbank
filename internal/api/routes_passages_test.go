package api

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
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

func TestPassageResolveErrorMapsSectionFailures(t *testing.T) {
	for _, test := range []struct {
		cause  error
		status int
		code   string
	}{
		{processing.ErrSectionNotFound, http.StatusNotFound, "section_unavailable"},
		{processing.ErrSectionContinuation, http.StatusConflict, "section_changed"},
		{processing.ErrSectionBudget, http.StatusUnprocessableEntity, "section_budget_too_small"},
		{processing.ErrOutlineTooLarge, http.StatusUnprocessableEntity, "outline_too_large"},
	} {
		mapped := passageResolveError(test.cause)
		var problem *Error
		require.ErrorAs(t, mapped, &problem)
		assert.Equal(t, test.status, problem.Status)
		assert.Equal(t, test.code, problem.Code)
	}
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

func TestPassageOutlineFromProcessingOwnsNestedSlicesAndLocators(t *testing.T) {
	locator := &document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
		IndexOrigin: document.EvidenceIndexOriginOne, Start: 3, End: 3}
	sections := []processing.OutlineSection{{Key: "parent", Title: "Parent", Level: 1,
		SourceLocator: locator, Children: []processing.OutlineSection{{Key: "child", Title: "Child", Level: 2}}}}
	wire := passageOutlineFromProcessing(processing.PassageOutline{
		BodySHA256: strings.Repeat("a", 64), RenditionBuildID: strings.Repeat("b", 64), Sections: sections,
	})
	sections[0].Title = "changed"
	sections[0].Children[0].Title = "changed"
	locator.Start = 99
	require.Len(t, wire.Sections, 1)
	assert.Equal(t, "Parent", wire.Sections[0].Title)
	assert.Equal(t, "Child", wire.Sections[0].Children[0].Title)
	require.NotNil(t, wire.Sections[0].SourceLocator)
	assert.Equal(t, int64(3), wire.Sections[0].SourceLocator.Start)
}
