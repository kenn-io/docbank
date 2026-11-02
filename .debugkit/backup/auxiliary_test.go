package backup

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestValidateAuxiliaryArtifactsRejectsAmbiguousAuthority(t *testing.T) {
	t.Parallel()
	open := func(context.Context) (io.ReadCloser, int64, error) {
		return io.NopCloser(strings.NewReader("")), 0, nil
	}
	tests := []struct {
		name      string
		artifacts []AuxiliaryArtifact
		want      string
	}{
		{
			name: "traversal name",
			artifacts: []AuxiliaryArtifact{{
				Name: "../placement", Format: "test-v1", Open: open,
			}},
			want: "invalid auxiliary artifact name",
		},
		{
			name: "duplicate name",
			artifacts: []AuxiliaryArtifact{
				{Name: "placement", Format: "test-v1", Open: open},
				{Name: "placement", Format: "test-v2", Open: open},
			},
			want: "duplicate auxiliary artifact name",
		},
		{
			name: "missing opener",
			artifacts: []AuxiliaryArtifact{{
				Name: "placement", Format: "test-v1",
			}},
			want: "has no opener",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.ErrorContains(t, validateAuxiliaryArtifacts(test.artifacts), test.want)
		})
	}
}

func TestValidateManifestAuxiliaryRequiresSortedBoundedIdentity(t *testing.T) {
	t.Parallel()
	id := blobID("auxiliary").String()
	valid := ManifestAuxiliary{
		Name: "placement", Format: "test-v1",
		Blob: id, Bytes: 9, SHA256: id,
	}
	tests := []struct {
		name      string
		artifacts []ManifestAuxiliary
		want      string
	}{
		{
			name: "unsorted",
			artifacts: []ManifestAuxiliary{
				{Name: "z", Format: "test-v1", Blob: id, SHA256: id},
				{Name: "a", Format: "test-v1", Blob: id, SHA256: id},
			},
			want: "not uniquely sorted",
		},
		{
			name: "digest mismatch",
			artifacts: []ManifestAuxiliary{{
				Name: "placement", Format: "test-v1",
				Blob: id, SHA256: blobID("other").String(),
			}},
			want: "digest differs",
		},
		{
			name: "aggregate too large",
			artifacts: []ManifestAuxiliary{
				{Name: "a", Format: "test-v1", Blob: id, Bytes: maxAuxiliaryBytes, SHA256: id},
				{Name: "b", Format: "test-v1", Blob: id, Bytes: 1, SHA256: id},
			},
			want: "invalid size",
		},
	}
	require.NoError(t, validateManifestAuxiliary([]ManifestAuxiliary{valid}))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.ErrorContains(t, validateManifestAuxiliary(test.artifacts), test.want)
		})
	}
}

func TestRestoreAuxiliaryRejectsOversizedFooterBeforePayloadRead(t *testing.T) {
	for _, corruptPayload := range []bool{false, true} {
		name := "valid payload"
		if corruptPayload {
			name = "unreadable payload"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			repo := initTestRepo(t)
			known := map[pack.BlobID]IndexEntry{}
			appender := NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, testPackExt)
			content := bytes.Repeat([]byte("oversized auxiliary payload"), 4096)
			id, _, err := appender.Add(content)
			require.NoError(err)
			_, _, err = appender.Finish()
			require.NoError(err)
			if corruptPayload {
				corruptStoredBlob(t, repo, known, id)
			}
			state := &restoreState{repo: repo, app: newTestApp(), known: known}
			manifest := &Manifest{Auxiliary: []ManifestAuxiliary{{
				Name: "placement", Format: "test-v1", Blob: id.String(),
				Bytes: 1, SHA256: id.String(),
			}}}

			restored, err := state.restoreAuxiliary(t.Context(), manifest)

			require.ErrorContains(err, "is 110592 bytes but manifest records 1")
			assert.Nil(t, restored)
		})
	}
}
