// Package embedassets owns verified embedding downloads and the pinned model cache.
package embedassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const revision = "e9b6763023c676ca8431644204f50c2b100d9aab"

// Asset describes a download with an exact size and SHA-256 digest.
type Asset struct {
	Name, URL, Digest string
	Size              int64
}

var modelAssets = []Asset{
	{"model.onnx", "onnx/model_fp16.onnx", "cf5b5a86edb00f895561803cfc04729090a958340b8ca2ad76c143f565f6bb04", 273859028},
	{"tokenizer.json", "tokenizer.json", "d241a60d5e8f04cc1b2b3e9ef7a4921b27bf526d9f6050ab90f9267a1f9e5c66", 711396},
}

// CacheModel fetches missing or corrupt pinned files and returns their directory.
func CacheModel(ctx context.Context, dir string) (string, error) {
	dir = ModelDir(dir)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	client := DownloadClient()
	defer client.CloseIdleConnections()
	for _, a := range ModelAssets() {
		if err := CacheAsset(ctx, client, dir, a); err != nil {
			return "", fmt.Errorf("cache %s: %w", a.Name, err)
		}
	}
	return dir, nil
}

func verify(r io.Reader, w io.Writer, a Asset) error {
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(r, a.Size+1))
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.Digest {
		return errors.New("asset size or SHA-256 mismatch")
	}
	return nil
}

// DownloadClient bounds response headers while the caller controls download lifetime.
func DownloadClient() *http.Client {
	client := &http.Client{}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := defaults.Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		client.Transport = transport
	}
	return client
}

func valid(path string, a Asset) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	verified := verify(file, io.Discard, a) == nil
	return verified, file.Close()
}

// CacheAsset reuses a verified file or atomically replaces it with a verified download.
func CacheAsset(ctx context.Context, client *http.Client, dir string, a Asset, opts ...atomicfile.Option) (err error) {
	path := filepath.Join(dir, a.Name)
	if verified, err := valid(path, a); err != nil || verified {
		return err
	}
	return DownloadAsset(ctx, client, dir, a, opts...)
}

// DownloadAsset verifies and publishes a download after the caller inspected the cache.
func DownloadAsset(ctx context.Context, client *http.Client, dir string, a Asset, opts ...atomicfile.Option) (err error) {
	path := filepath.Join(dir, a.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", res.StatusCode)
	}
	file, err := atomicfile.Create(path, append(opts, atomicfile.WithPrivate())...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Abort()) }()
	if err := verify(res.Body, file, a); err != nil {
		return err
	}
	return file.Commit()
}

// ModelDir is the revision-specific cache directory shared with the server.
func ModelDir(dir string) string { return filepath.Join(dir, "nomic-embed-text-v1.5", revision) }

// ModelAssets returns the pinned downloads with their absolute URLs.
func ModelAssets() []Asset {
	assets := append([]Asset(nil), modelAssets...)
	for i := range assets {
		assets[i].URL = "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/" + revision + "/" + assets[i].URL
	}
	return assets
}

// MissingModel inspects files once without writing or making network requests.
// It returns the assets whose cached bytes need replacement.
func MissingModel(dir string, assets []Asset) ([]Asset, error) {
	var missing []Asset
	for _, a := range assets {
		verified, err := valid(filepath.Join(dir, a.Name), a)
		if err != nil {
			return nil, err
		}
		if !verified {
			missing = append(missing, a)
		}
	}
	return missing, nil
}
