package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

var addAllowHTTP bool
var addMaxBytes int64

type urlDownload struct {
	file                   *os.File
	name, digest, mimeType string
	size                   int64
	fetch                  ingest.URLFetch
}

func fetchURL(ctx context.Context, client http.Client, raw string, allowHTTP bool, maxBytes int64) (_ *urlDownload, retErr error) {
	if maxBytes < 1 || maxBytes > blob.MaxIngestBytes {
		return nil, fmt.Errorf("--max-bytes must be between 1 and %d", blob.MaxIngestBytes)
	}
	validate := func(u *url.URL) error {
		if u.Host == "" || (u.Scheme != "https" && (!allowHTTP || u.Scheme != "http")) {
			return errors.New("URL requires HTTPS; use --allow-http to allow HTTP")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	if err := validate(u); err != nil {
		return nil, err
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return validate(req.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch returned HTTP %d", resp.StatusCode)
	}
	file, err := os.CreateTemp("", "docbank-url-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	hash := sha256.New()
	size, prefix, err := hashPutSource(io.LimitReader(resp.Body, maxBytes+1), io.MultiWriter(file, hash))
	if err != nil {
		return nil, fmt.Errorf("downloading: %w", err)
	}
	if size > maxBytes {
		return nil, fmt.Errorf("download exceeds %d bytes", maxBytes)
	}
	fetch := ingest.URLFetch{Version: 1, RequestedURL: raw, FinalURL: resp.Request.URL.String(), FetchedAt: time.Now().UTC().Format(time.RFC3339Nano), ETag: []byte(resp.Header.Get("ETag")), LastModified: []byte(resp.Header.Get("Last-Modified"))}
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	var name string
	for _, candidate := range []string{params["filename"], path.Base(resp.Request.URL.Path), path.Base(u.Path), "download"} {
		if normalized, err := store.NormalizeName(candidate); err == nil && !strings.ContainsAny(normalized, "\r\n") {
			name = normalized
			break
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return &urlDownload{file: file, name: name, digest: hex.EncodeToString(hash.Sum(nil)), mimeType: ingest.DetectMIME(name, prefix), size: size, fetch: fetch}, nil
}

func addURL(cmd *cobra.Command, raw string) (rep api.IngestReport, retErr error) {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	download, err := fetchURL(ctx, http.Client{}, raw, addAllowHTTP, addMaxBytes)
	if err != nil {
		return rep, err
	}
	defer func() { _ = download.file.Close(); _ = os.Remove(download.file.Name()) }()
	c, err := daemonconn.Ensure(ctx)
	if err != nil {
		return rep, err
	}
	defer func() {
		if !isClosedOutputPipe(retErr) {
			reportFailure(cmd, c, "add", retErr, false)
		}
	}()
	receipt, err := c.UploadURL(ctx, addDest, download.name, download.mimeType, download.digest, download.size, download.file, download.fetch)
	if err != nil {
		return rep, err
	}
	if receipt.Status == "added" {
		rep.Added = 1
		rep.IngestID = receipt.IngestID
		reportAction(cmd, c, "document_added", "", "", false)
	} else {
		rep.Skipped = 1
	}
	return rep, nil
}

func isURLSource(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}
