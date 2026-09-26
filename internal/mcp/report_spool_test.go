package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

type reportEOFSignalReader struct {
	io.Reader

	once sync.Once
	done chan<- struct{}
}

func (reader *reportEOFSignalReader) Read(p []byte) (int, error) {
	n, err := reader.Reader.Read(p)
	if err == io.EOF {
		reader.once.Do(func() { reader.done <- struct{}{} })
	}
	return n, err
}

func (*reportEOFSignalReader) Close() error { return nil }

func TestReportBundleVerificationGateBoundsConcurrentCompressedOpens(t *testing.T) {
	var encoded bytes.Buffer
	archive := zip.NewWriter(&encoded)
	for _, name := range []string{"hits.csv", "manifest.json", "members.jsonl", "families.jsonl", "dates.jsonl"} {
		entry, err := archive.Create(name)
		require.NoError(t, err)
		payload := []byte("{}")
		if name == "hits.csv" {
			payload = bytes.Repeat([]byte("x"), 16<<20)
		}
		_, err = entry.Write(payload)
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	packet := encoded.Bytes()
	require.Less(t, len(packet), 128<<10, "the fixture must exercise compressed-size undercharging")

	// Hold the process-wide verifier slot while two independent opens finish
	// streaming the small ZIP. Neither may start decompressing its 16 MiB
	// member until the slot is released.
	release, err := acquireReportBundleVerification(t.Context())
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	type result struct {
		spool reportSpool
		err   error
	}
	copyFinished := make(chan struct{}, 2)
	results := make(chan result, 2)
	sum := sha256.Sum256(packet)
	for range 2 {
		stream := &daemonconn.TermReportStream{
			ReadCloser: &reportEOFSignalReader{Reader: bytes.NewReader(packet), done: copyFinished},
			Size:       int64(len(packet)), SHA256: hex.EncodeToString(sum[:]),
		}
		go func() {
			spool, err := createVerifiedReportSpool(t.Context(), stream, "bundle")
			results <- result{spool: spool, err: err}
		}()
	}
	for range 2 {
		select {
		case <-copyFinished:
		case <-time.After(5 * time.Second):
			t.Fatal("a compressed bundle did not finish streaming before verification")
		}
	}
	select {
	case got := <-results:
		cleanupReportSpool(&got.spool)
		t.Fatalf("bundle verification started while the process-wide slot was held: %v", got.err)
	case <-time.After(time.Second):
	}
	release()
	released = true
	for range 2 {
		select {
		case got := <-results:
			cleanupReportSpool(&got.spool)
			require.ErrorIs(t, got.err, report.ErrInvalidPacket)
		case <-time.After(5 * time.Second):
			t.Fatal("a bundle verifier did not release its process-wide slot")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	nextRelease, err := acquireReportBundleVerification(ctx)
	require.NoError(t, err, "all verifier permits must return after failed opens")
	nextRelease()
}

func TestReportBundleVerificationWaitCancelAndSuccessReleaseSlot(t *testing.T) {
	privateTemp := t.TempDir()
	t.Setenv("TMPDIR", privateTemp)
	t.Setenv("TMP", privateTemp)
	t.Setenv("TEMP", privateTemp)
	packet := syntheticVerifiedReportBundle(t)
	sum := sha256.Sum256(packet)
	stream := func(reader io.ReadCloser) *daemonconn.TermReportStream {
		return &daemonconn.TermReportStream{
			ReadCloser: reader, Size: int64(len(packet)), SHA256: hex.EncodeToString(sum[:]),
		}
	}
	release, err := acquireReportBundleVerification(t.Context())
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	copyFinished := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		spool, err := createVerifiedReportSpool(ctx, stream(&reportEOFSignalReader{
			Reader: bytes.NewReader(packet), done: copyFinished,
		}), "bundle")
		cleanupReportSpool(&spool)
		result <- err
	}()
	select {
	case <-copyFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("the bundle did not finish streaming before verification")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceling a verifier wait did not return promptly")
	}
	remaining, err := os.ReadDir(privateTemp)
	require.NoError(t, err)
	require.Empty(t, remaining, "canceled verification must remove its private spool")
	release()
	released = true
	spool, err := createVerifiedReportSpool(t.Context(), stream(io.NopCloser(bytes.NewReader(packet))), "bundle")
	require.NoError(t, err)
	cleanupReportSpool(&spool)
	nextRelease, err := acquireReportBundleVerification(t.Context())
	require.NoError(t, err, "successful verification must release the process-wide slot")
	nextRelease()
}

func TestReportSpoolCapacityBoundsConcurrentOpenAndReleasesReservation(t *testing.T) {
	signer := newReportHandleSigner()
	require.NoError(t, signer.reserve(maxReportSpoolBytes))
	require.ErrorIs(t, signer.reserve(1), errReportSpoolCapacity)
	signer.abort(maxReportSpoolBytes)
	for range maxOpenReportHandles {
		require.NoError(t, signer.reserve(1))
	}
	require.ErrorIs(t, signer.reserve(1), errReportSpoolCapacity)
	for range maxOpenReportHandles {
		signer.abort(1)
	}
	require.Zero(t, signer.reservedBytes)
	require.Zero(t, signer.opening)
}

func TestReportSpoolTimerAndExplicitCloseRemovePrivateFiles(t *testing.T) {
	signer := newReportHandleSigner()
	t.Cleanup(signer.closeAll)
	create := func(expires int64) (string, string, reportHandle) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "synthetic-report")
		data := []byte("synthetic verified report")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		file, err := os.Open(path)
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		metadata := reportHandle{ID: strings.Repeat("a", 48), Format: "csv", Size: int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]), Expires: expires}
		token, err := signer.sign(&metadata)
		require.NoError(t, err)
		require.NoError(t, signer.reserve(metadata.Size))
		require.NoError(t, signer.publish(token, &reportSpool{
			metadata: metadata, file: file, path: path, size: metadata.Size,
		}))
		return token, path, metadata
	}
	first, firstPath, firstMetadata := create(time.Now().Unix() + 60)
	chunk, err := signer.read(first, firstMetadata, 0, 9, true)
	require.NoError(t, err)
	require.Equal(t, "synthetic", string(chunk))
	_, statErr := os.Stat(firstPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	_, secondPath, _ := create(time.Now().Unix() + 2)
	require.Eventually(t, func() bool {
		signer.mu.Lock()
		remaining, bytes := len(signer.spools), signer.reservedBytes
		signer.mu.Unlock()
		_, statErr := os.Stat(secondPath)
		return remaining == 0 && bytes == 0 && os.IsNotExist(statErr)
	}, 3*time.Second, 10*time.Millisecond)
}

func TestReportSpoolShutdownDiscardsInFlightOpen(t *testing.T) {
	signer := newReportHandleSigner()
	path := filepath.Join(t.TempDir(), "synthetic-report")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	file, err := os.Open(path)
	require.NoError(t, err)
	metadata := reportHandle{ID: strings.Repeat("a", 48), Format: "csv", Size: 1,
		SHA256: strings.Repeat("b", 64), Expires: time.Now().Add(time.Minute).Unix()}
	token, err := signer.sign(&metadata)
	require.NoError(t, err)
	require.NoError(t, signer.reserve(1))
	signer.closeAll()
	require.ErrorIs(t, signer.publish(token, &reportSpool{
		metadata: metadata, file: file, path: path, size: 1,
	}), errReportHandleUnavailable)
	require.Zero(t, signer.reservedBytes)
	require.Zero(t, signer.opening)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorIs(t, signer.reserve(1), errReportHandleUnavailable)
}
