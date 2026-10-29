package store

import (
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

type embeddingLiveSnapshot struct {
	current        bool
	nodeID         int64
	kind           string
	textCompatible bool
}

// A failed compatibility check disables only collection, never source validation.
// Keep the guard local to one capture; SQL remains the authority for mixed classes.
type semanticSnapshotValueGuard struct{ compatible bool }

func (g *semanticSnapshotValueGuard) include(compatible bool) {
	g.compatible = g.compatible && compatible
}

type vectorIndexSourceObserver func(context.Context, vectorIndexHeadCandidate, EmbeddingSetRecord, embeddingLiveSnapshot, bool) error

type originalSemanticSnapshot struct {
	profile, binding, space string
	usable                  bool
	versions                map[string]struct{}
	completeNodes           map[int64]struct{}
	annRows                 []vectorindex.RowIdentity
	seenANN                 map[semanticEligibilityKey]struct{}
}

func newOriginalSemanticSnapshot(profile, binding, space string, kind document.EmbeddingInputKind, opts SearchOptions, wantANN bool) *originalSemanticSnapshot {
	if kind != document.EmbeddingInputOriginalFile || len(opts.SelectedBuilds) != 0 {
		return nil
	}
	metadata := opts
	metadata.ContentVersionIDs = nil
	if query, _ := searchFilterSQL(metadata); query != "" {
		return nil
	}
	c := &originalSemanticSnapshot{usable: true, profile: profile, binding: binding, space: space, completeNodes: make(map[int64]struct{})}
	if len(opts.ContentVersionIDs) != 0 {
		c.versions = make(map[string]struct{}, len(opts.ContentVersionIDs))
		for _, id := range opts.ContentVersionIDs {
			c.versions[id] = struct{}{}
		}
	}
	if wantANN {
		c.annRows = make([]vectorindex.RowIdentity, 0)
		c.seenANN = make(map[semanticEligibilityKey]struct{})
	}
	return c
}

func (c *originalSemanticSnapshot) observe(ctx context.Context, head vectorIndexHeadCandidate, set EmbeddingSetRecord, live embeddingLiveSnapshot, textCompatible bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !textCompatible {
		c.usable = false
		c.completeNodes = nil
		c.annRows = nil
		c.seenANN = nil
	}
	if !c.usable {
		return nil
	}
	if !live.current || !head.contentVersion.Valid || !head.binding.Valid || !head.inputKind.Valid ||
		head.setID != set.ID || head.contentVersion.String != set.ContentVersionID || head.binding.String != set.BindingID || head.inputKind.String != string(set.InputKind) || head.profile != set.ProcessingProfileFingerprint || head.vectorSpace != set.VectorSpace.ID ||
		set.ProcessingProfileFingerprint != c.profile || set.BindingID != c.binding || set.VectorSpace.ID != c.space || set.InputKind != document.EmbeddingInputOriginalFile {
		return nil
	}
	if c.versions != nil {
		if _, ok := c.versions[set.ContentVersionID]; !ok {
			return nil
		}
	}
	if live.kind == "file" {
		c.completeNodes[live.nodeID] = struct{}{}
	}
	if c.seenANN == nil {
		return nil
	}
	inputs := make(map[string]string, len(set.InputGeneration.Inputs))
	for i, input := range set.InputGeneration.Inputs {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		inputs[input.ID] = input.RenderedChecksum
	}
	for i, row := range set.VectorSet.rows {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		checksum, ok := inputs[row.InputID]
		if !ok || checksum != row.Checksum {
			continue
		}
		key := semanticEligibilityKey{VectorSetID: set.VectorSet.ID, InputID: row.InputID, InputChecksum: row.Checksum}
		if _, duplicate := c.seenANN[key]; duplicate {
			continue
		}
		c.seenANN[key] = struct{}{}
		c.annRows = append(c.annRows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
	}
	return nil
}
