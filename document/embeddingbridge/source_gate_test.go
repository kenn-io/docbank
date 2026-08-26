package embeddingbridge

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestActiveSourceCancellationBetweenFilesLeavesBothSourcesOpen(t *testing.T) {
	first := &gateTestUpload{}
	second := &gateTestUpload{}
	gate := newActiveSourceGate()

	firstToken, ok := gate.Begin(first)
	require.True(t, ok)
	gate.End(firstToken)
	gate.Cancel()
	_, ok = gate.Begin(second)
	assert.False(t, ok)
	assert.Zero(t, first.closes.Load())
	assert.Zero(t, second.closes.Load())
}

type gateTestUpload struct {
	closes atomic.Int32
}

func (*gateTestUpload) Read([]byte) (int, error) { return 0, nil }

func (*gateTestUpload) Metadata() document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{}
}

func (upload *gateTestUpload) Close() error {
	upload.closes.Add(1)
	return nil
}
