package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestSetupLocalFacesSelectsFaceModelAndStartsInstalledServer(t *testing.T) {
	dir := t.TempDir()
	var output bytes.Buffer
	endpoints := 0
	endpoint := func(_ context.Context, opts embedlocal.Options) (string, error) {
		assert.Equal(t, dir, opts.DataDir)
		assert.Equal(t, embedlocal.FaceModel, opts.Model)
		assert.True(t, opts.SkipOllama)
		endpoints++
		if endpoints == 1 {
			assert.Empty(t, opts.Executable)
			return "", embedlocal.ErrNotFound
		}
		assert.Equal(t, "synthetic-installed-server", opts.Executable)
		return "http://127.0.0.1:12345/v1", nil
	}
	install := func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		assert.Equal(t, dir, opts.DataDir)
		assert.Equal(t, embedlocal.FaceModel, opts.Model)
		ok, err := opts.Confirm(ctx, 260926740)
		require.NoError(t, err)
		require.True(t, ok)
		return embedlocal.Installation{DataDir: dir, Executable: "synthetic-installed-server"}, nil
	}
	require.NoError(t, setupLocalFaces(t.Context(), &output, dir, endpoint, install))
	assert.Contains(t, output.String(), "260926740 bytes")
	assert.Contains(t, output.String(), "ready")
	assert.Equal(t, 2, endpoints)
}

func TestSetupLocalFacesPreservesExistingServerFailure(t *testing.T) {
	dir := t.TempDir()
	for _, problem := range []error{nil, &embedlocal.RestartRequiredError{PID: 123}} {
		var output bytes.Buffer
		endpoint := func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", problem }
		install := func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
			assert.Fail(t, "ready or incompatible existing server must not trigger installation")
			return embedlocal.Installation{}, errors.New("unexpected install")
		}
		err := setupLocalFaces(t.Context(), &output, dir, endpoint, install)
		if problem == nil {
			assert.NoError(t, err)
		} else {
			assert.ErrorIs(t, err, problem)
		}
	}
}
