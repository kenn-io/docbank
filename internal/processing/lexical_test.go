package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLexicalGenerationRebuildRemainsUnreachableUntilPublication(t *testing.T) {
	// Mutation caught: rebuilding a generation must not flip the lexical head;
	// only the later atomic attachment/rendition/lexical publication may do so.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	first, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "prior mercury evidence", "first markdown",
	))
	require.NoError(t, err)

	failingCatalog := &failAfterCatalogStage{renditionPublicationCatalog: fixture.catalog}
	failingPublisher, err := NewArtifactPublisher(failingCatalog, fixture.blobs)
	require.NoError(t, err)
	_, err = failingPublisher.PublishRendition(t.Context(), fixture.replacementStage(t,
		publicationIDs{"b2", "52", "92"}, "replacement venus evidence", "second markdown",
	))
	require.ErrorIs(t, err, errInjectedPublication)

	rebuilt, err := RebuildLexicalGeneration(t.Context(), fixture.catalog, processingHash("92"))
	require.NoError(t, err)
	assert.Equal(t, processingHash("92"), rebuilt.ID)
	assert.Positive(t, rebuilt.SegmentCount)
	active, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first.LexicalGeneration, active)

	hits, _, err := fixture.catalog.SearchPage(t.Context(), "venus", 10)
	require.NoError(t, err)
	assert.Empty(t, hits, "an unreachable generation cannot serve")
}
