package notebook

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/store"
	"hegel.dev/go/hegel"
)

// Resolve permutations against a real sealed namespace, without recreating a
// database per example. The authored table is the oracle; neither the selector
// nor the parser computes expected targets, states, or heading spans. Process
// persistence and mutation races are covered by the real-store named tests.
func TestNoteProjectorEdgePermutationProperty(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	const prefix = "---\naliases: [shared, twins]\n---\n"
	const targetBody = prefix + "# Target\nBody\n"
	refs := []string{"source.md", "a.md", "b.md", "twin.md", "terminal.md", ".md"}
	bodies := []string{"source", targetBody, "---\naliases: [shared]\n---\n# Other\n", targetBody, "\x00", "path only"}
	ids := make(map[string]store.PublishedNoteIdentity)
	records := make(map[string]store.NoteParseRecord)
	for i, ref := range refs {
		ids[ref] = f.accept(t, scan, ref, bodies[i], 0)
	}
	f.complete(t, scan)
	require.Equal(t, ids["a.md"].Hash, ids["twin.md"].Hash)
	require.NotEqual(t, ids["a.md"].NodeID, ids["twin.md"].NodeID)
	require.NotEqual(t, ids["a.md"].ContentVersionID, ids["twin.md"].ContentVersionID)
	for i, ref := range refs {
		id := ids[ref]
		if ref == "terminal.md" {
			record := store.NoteParseRecord{ContentVersionID: id.ContentVersionID,
				Hash: id.Hash, Size: id.Size, Recipe: notemarkdown.Recipe,
				State: "failed", Reason: "invalid_markdown"}
			require.NoError(t, f.catalog.RecordNoteParse(t.Context(), record))
			records[ref] = record
		} else {
			// Parsing seeds a valid cache precondition, never the expected edges.
			records[ref] = f.seedParse(t, id, bodies[i])
		}
	}
	generation, err := f.catalog.BeginNoteIndex(t.Context(), f.target(t), notemarkdown.Recipe, notemarkdown.ResolverRecipe)
	require.NoError(t, err)
	for _, ref := range refs {
		id := ids[ref]
		id.NamespaceRevision = generation.NamespaceRevision
		ids[ref] = id
		record := records[ref]
		var note notemarkdown.Note
		if record.State == "ready" {
			require.NoError(t, json.Unmarshal(record.Artifact, &note))
		}
		require.NoError(t, f.catalog.StageNoteIndexSource(t.Context(), generation,
			store.NotePublicationSource{Identity: id}, record, lookupKeys(ref, note)))
	}
	require.NoError(t, f.catalog.SealNoteIndexKeys(t.Context(), generation))
	heading := notemarkdown.Span{Start: len(prefix), End: len(prefix) + len("# Target\n")}
	cases := []struct {
		raw, state, target string
		heading            *notemarkdown.Span
	}{
		{"a.md", "resolved", "a.md", nil},
		{"b.md", "resolved", "b.md", nil},
		{"twin.md", "resolved", "twin.md", nil},
		{"shared", "ambiguous", "", nil},
		{"shared#target", "ambiguous", "", nil},
		{"twins", "ambiguous", "", nil},
		{"twins#target", "ambiguous", "", nil},
		{"SHARED", "missing", "", nil},
		{"missing.md", "missing", "", nil},
		{"a.md#target", "resolved", "a.md", &heading},
		{"a.md#absent", "missing", "a.md", nil},
		{"terminal.md", "resolved", "terminal.md", nil},
		{"terminal.md#target", "unavailable", "terminal.md", nil},
		{".md", "resolved", ".md", nil},
	}
	projector := NewProjector(f.catalog, f.blobs)
	hegel.Test(t, func(ht *hegel.T) {
		order := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, len(cases)-1)))
		if len(order) > 16 {
			order = order[:16]
		}
		links := make([]notemarkdown.Link, 0, len(order))
		want := make([]store.NoteIndexEdge, 0, len(order))
		offset := 0
		for ordinal, index := range order {
			item := cases[index]
			embed := hegel.Draw(ht, hegel.Booleans())
			prefixBytes := 2
			if embed {
				prefixBytes++
			}
			link := notemarkdown.Link{Span: notemarkdown.Span{Start: offset, End: offset + prefixBytes + len(item.raw) + 2},
				DestinationSpan: notemarkdown.Span{Start: offset + prefixBytes, End: offset + prefixBytes + len(item.raw)},
				Form:            "wiki", Embed: embed, RawTarget: item.raw, Target: item.raw}
			links = append(links, link)
			edge := store.NoteIndexEdge{Ordinal: int64(ordinal), Link: link, State: item.state, TargetHeading: item.heading}
			if item.target != "" {
				id := ids[item.target]
				edge.Target = &id
			}
			want = append(want, edge)
			offset = link.Span.End + 1
		}
		got, err := projector.edges(ht.Context(), generation, "source.md", links)
		require.NoError(ht, err)
		require.Equal(ht, want, got)
	}, hegel.WithDatabase(t.TempDir()), hegel.WithSeed(2026110802), hegel.WithTestCases(2000))
}
