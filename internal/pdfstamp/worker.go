package pdfstamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sync"
)

const maxWorkerRequestBytes = 1 << 20

var workerConfig struct {
	sync.RWMutex

	executable string
}

type workerRequest struct {
	Operation string      `json:"operation"`
	Labels    []PageLabel `json:"labels"`
	Pages     []int       `json:"pages"`
	Recipe    Recipe      `json:"recipe"`
	Sources   int         `json:"sources"`
}

// ConfigureWorker makes transforming operations use the named supervised
// helper process. An empty path keeps library callers on the synchronous API.
func ConfigureWorker(executable string) {
	workerConfig.Lock()
	workerConfig.executable = executable
	workerConfig.Unlock()
}

func configuredWorker() string {
	workerConfig.RLock()
	defer workerConfig.RUnlock()
	return workerConfig.executable
}

func StampSelectedSupervised(ctx context.Context, source io.ReadSeeker, labels []PageLabel, recipe Recipe, output io.Writer) (SelectedResult, error) {
	if configuredWorker() == "" {
		return StampSelected(ctx, source, labels, recipe, output)
	}
	data, err := readWorkerSource(source)
	if err != nil {
		return SelectedResult{}, err
	}
	result, err := runWorker(ctx, workerRequest{Operation: "stamp_selected", Labels: labels, Recipe: recipe, Sources: 1}, [][]byte{data})
	if err != nil {
		return SelectedResult{}, err
	}
	if err := writeSelectedOutput(output, result); err != nil {
		return SelectedResult{}, err
	}
	pageMap := make([]SourceOutputPage, len(labels))
	for index, label := range labels {
		pageMap[index] = SourceOutputPage{SourcePage: label.SourcePage, OutputPage: index + 1}
	}
	digest := sha256.Sum256(result)
	return SelectedResult{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: len(labels), PageMap: pageMap}, nil
}

func SelectPagesSupervised(ctx context.Context, source io.ReadSeeker, pages []int, output io.Writer) (SelectedResult, error) {
	if configuredWorker() == "" {
		return SelectPages(ctx, source, pages, output)
	}
	data, err := readWorkerSource(source)
	if err != nil {
		return SelectedResult{}, err
	}
	result, err := runWorker(ctx, workerRequest{Operation: "select", Pages: pages, Sources: 1}, [][]byte{data})
	if err != nil {
		return SelectedResult{}, err
	}
	if err := writeSelectedOutput(output, result); err != nil {
		return SelectedResult{}, err
	}
	pageMap := make([]SourceOutputPage, len(pages))
	for index, page := range pages {
		pageMap[index] = SourceOutputPage{SourcePage: page, OutputPage: index + 1}
	}
	digest := sha256.Sum256(result)
	return SelectedResult{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: len(pages), PageMap: pageMap}, nil
}

func CombineStampedSupervised(ctx context.Context, groups [][]byte, labels []PageLabel, output io.Writer) (Result, error) {
	if configuredWorker() == "" {
		return CombineStamped(ctx, groups, labels, output)
	}
	result, err := runWorker(ctx, workerRequest{Operation: "combine", Labels: labels, Sources: len(groups)}, groups)
	if err != nil {
		return Result{}, err
	}
	if err := writeSelectedOutput(output, result); err != nil {
		return Result{}, err
	}
	digest := sha256.Sum256(result)
	return Result{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: len(labels), Pages: slices.Clone(labels)}, nil
}

func readWorkerSource(source io.ReadSeeker) ([]byte, error) {
	if source == nil {
		return nil, stampFailure("read worker source", errors.New("nil source"))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, stampFailure("read worker source", err)
	}
	data, err := io.ReadAll(io.LimitReader(source, maxStampOutputBytes+1))
	if err != nil || int64(len(data)) > maxStampOutputBytes {
		return nil, stampFailure("read worker source", errors.Join(err, ErrStampEngineFailure))
	}
	return data, nil
}

func runWorker(ctx context.Context, request workerRequest, sources [][]byte) ([]byte, error) {
	executable := configuredWorker()
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maxWorkerRequestBytes || request.Sources != len(sources) {
		return nil, stampFailure("encode worker request", err)
	}
	var input bytes.Buffer
	if err := binary.Write(&input, binary.BigEndian, uint64(len(encoded))); err != nil {
		return nil, fmt.Errorf("encode worker request length: %w", err)
	}
	_, _ = input.Write(encoded)
	for _, source := range sources {
		if len(source) == 0 || int64(len(source)) > maxStampOutputBytes {
			return nil, stampFailure("encode worker source", ErrStampEngineFailure)
		}
		_ = binary.Write(&input, binary.BigEndian, uint64(len(source)))
		_, _ = input.Write(source)
	}
	command := exec.CommandContext(ctx, executable, "internal-pdfstamp-worker") //nolint:gosec // executable is the running Docbank binary.
	command.Stdin = &input
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedStampWriter{Writer: &stdout, Remaining: maxStampOutputBytes}
	command.Stderr = &limitedStampWriter{Writer: &stderr, Remaining: 64 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, stampFailure("supervised PDF worker", fmt.Errorf("%w: %s", err, stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, stampFailure("supervised PDF worker", ErrStampEngineFailure)
	}
	return stdout.Bytes(), nil
}

// RunWorker serves one framed transformation request on private process pipes.
func RunWorker(ctx context.Context, input io.Reader, output io.Writer) error {
	var requestSize uint64
	if err := binary.Read(input, binary.BigEndian, &requestSize); err != nil || requestSize == 0 || requestSize > maxWorkerRequestBytes {
		return stampFailure("decode worker request", err)
	}
	requestBytes := make([]byte, requestSize)
	if _, err := io.ReadFull(input, requestBytes); err != nil {
		return err
	}
	var request workerRequest
	if err := json.Unmarshal(requestBytes, &request, json.RejectUnknownMembers(true)); err != nil || request.Sources < 1 || request.Sources > 100_000 {
		return stampFailure("decode worker request", err)
	}
	sources := make([][]byte, request.Sources)
	var total int64
	for index := range sources {
		var size uint64
		if err := binary.Read(input, binary.BigEndian, &size); err != nil || size == 0 || size > uint64(maxStampOutputBytes) {
			return stampFailure("decode worker source", err)
		}
		total += int64(size)
		if total > maxStampOutputBytes {
			return stampFailure("decode worker sources", ErrStampEngineFailure)
		}
		sources[index] = make([]byte, size)
		if _, err := io.ReadFull(input, sources[index]); err != nil {
			return err
		}
	}
	switch request.Operation {
	case "stamp_selected":
		_, err := StampSelected(ctx, bytes.NewReader(sources[0]), request.Labels, request.Recipe, output)
		return err
	case "select":
		_, err := SelectPages(ctx, bytes.NewReader(sources[0]), request.Pages, output)
		return err
	case "combine":
		_, err := CombineStamped(ctx, sources, request.Labels, output)
		return err
	default:
		return stampFailure("decode worker request", errors.New("unknown operation"))
	}
}
