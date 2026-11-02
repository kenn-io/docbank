package embedserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/internal/embedassets"
)

type engine interface {
	embed(context.Context, string) ([]float32, int, error)
	Close() error
}
type model struct {
	ort       *ort
	tokenizer *tokenizer
}

func loadModel(ctx context.Context, dir, library string) (engine, error) {
	library, err := filepath.Abs(library)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(library); err != nil {
		return nil, fmt.Errorf("ONNX Runtime library: %w", err)
	}
	cached, err := embedassets.CacheModel(ctx, dir)
	if err != nil {
		return nil, err
	}
	t, err := loadTokenizer(filepath.Join(cached, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	o, err := openORT(library, filepath.Join(cached, "model.onnx"))
	if err != nil {
		return nil, err
	}
	return &model{o, t}, nil
}
func (m *model) Close() error { return m.ort.Close() }
func (m *model) embed(ctx context.Context, text string) ([]float32, int, error) {
	ids := m.tokenizer.encode(text)
	vector, err := m.ort.infer(ctx, ids)
	return vector, len(ids), err
}
