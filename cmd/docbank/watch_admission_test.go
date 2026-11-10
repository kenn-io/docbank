package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchImportAdmissionRoutes(t *testing.T) {
	var got []string
	sentinel := errors.New("admission failed")
	ctx := t.Context()
	admission := watchImportAdmission(config.WatchConfig{Profiles: map[string]string{"document": "docling", "audio_video": "asr"}}, func(callCtx context.Context, node store.Node, profile string) error {
		require.Equal(t, ctx, callCtx)
		got = append(got, profile+":"+node.Name)
		if node.Name == "failed.pdf" {
			return sentinel
		}
		return nil
	})
	for _, node := range []store.Node{{Name: "report.pdf", MimeType: "application/pdf"}, {Name: "memo.wav", MimeType: "audio/wav"}, {Name: "notes.txt", MimeType: "text/plain"}} {
		require.NoError(t, admission(ctx, node))
	}
	require.Equal(t, []string{"docling:report.pdf", "asr:memo.wav"}, got)
	require.ErrorIs(t, admission(ctx, store.Node{Name: "failed.pdf", MimeType: "application/pdf"}), sentinel)
}
