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
	"math"
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
	pageCount, result, err := runWorker(ctx, workerRequest{Operation: "stamp_selected", Labels: labels, Recipe: recipe, Sources: 1}, [][]byte{data})
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
	return SelectedResult{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: pageCount, PageMap: pageMap}, nil
}

func SelectPagesSupervised(ctx context.Context, source io.ReadSeeker, pages []int, output io.Writer) (SelectedResult, error) {
	if configuredWorker() == "" {
		return SelectPages(ctx, source, pages, output)
	}
	data, err := readWorkerSource(source)
	if err != nil {
		return SelectedResult{}, err
	}
	pageCount, result, err := runWorker(ctx, workerRequest{Operation: "select", Pages: pages, Sources: 1}, [][]byte{data})
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
	return SelectedResult{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: pageCount, PageMap: pageMap}, nil
}

func CombineStampedSupervised(ctx context.Context, groups [][]byte, labels []PageLabel, output io.Writer) (Result, error) {
	if configuredWorker() == "" {
		return CombineStamped(ctx, groups, labels, output)
	}
	pageCount, result, err := runWorker(ctx, workerRequest{Operation: "combine", Labels: labels, Sources: len(groups)}, groups)
	if err != nil {
		return Result{}, err
	}
	if err := writeSelectedOutput(output, result); err != nil {
		return Result{}, err
	}
	digest := sha256.Sum256(result)
	return Result{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(result)), PageCount: pageCount, Pages: slices.Clone(labels)}, nil
}

func readWorkerSource(source io.ReadSeeker) ([]byte, error) {
	if source == nil {
		return nil, stampFailure("read worker source", errors.New("nil source"))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, stampFailure("read worker source", err)
	}
	data, err := io.ReadAll(io.LimitReader(source, MaxOutputBytes+1))
	if err != nil || int64(len(data)) > MaxOutputBytes {
		return nil, stampFailure("read worker source", errors.Join(err, ErrStampEngineFailure))
	}
	return data, nil
}

// runWorker returns the page count the worker verified and the PDF bytes it wrote.
func runWorker(ctx context.Context, request workerRequest, sources [][]byte) (int, []byte, error) {
	executable := configuredWorker()
	encoded, err := json.Marshal(request)
	if err != nil {
		return 0, nil, stampFailure("encode worker request", err)
	}
	if len(encoded) > maxWorkerRequestBytes {
		return 0, nil, stampFailure("encode worker request",
			fmt.Errorf("request is %d bytes; the limit is %d", len(encoded), maxWorkerRequestBytes))
	}
	if request.Sources != len(sources) {
		return 0, nil, stampFailure("encode worker request",
			fmt.Errorf("request declares %d sources but %d were supplied", request.Sources, len(sources)))
	}
	var input bytes.Buffer
	_ = binary.Write(&input, binary.BigEndian, uint64(len(encoded)))
	_, _ = input.Write(encoded)
	var total int64
	for _, source := range sources {
		total += int64(len(source))
		if len(source) == 0 || total > MaxOutputBytes {
			return 0, nil, stampFailure("encode worker source",
				fmt.Errorf("sources must be non-empty and total at most %d bytes", MaxOutputBytes))
		}
		_ = binary.Write(&input, binary.BigEndian, uint64(len(source)))
		_, _ = input.Write(source)
	}
	command := exec.CommandContext(ctx, executable, "internal-pdfstamp-worker") //nolint:gosec // executable is the running Docbank binary.
	command.Stdin = &input
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedStampWriter{Writer: &stdout, Remaining: MaxOutputBytes + 8}
	command.Stderr = &limitedStampWriter{Writer: &stderr, Remaining: 64 << 10}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, stampFailure("supervised PDF worker", fmt.Errorf("%w: %s", err, stderr.String()))
	}
	var pageCount uint64
	if err := binary.Read(&stdout, binary.BigEndian, &pageCount); err != nil || pageCount == 0 ||
		pageCount > math.MaxInt32 || stdout.Len() == 0 {
		return 0, nil, stampFailure("supervised PDF worker", errors.New("worker returned no verified PDF"))
	}
	return int(pageCount), stdout.Bytes(), nil
}

// RunWorker serves one framed transformation request on private process pipes.
// It writes the verified page count as a big-endian uint64, then the PDF.
func RunWorker(ctx context.Context, input io.Reader, output io.Writer) error {
	var requestSize uint64
	if err := binary.Read(input, binary.BigEndian, &requestSize); err != nil {
		return stampFailure("decode worker request", err)
	}
	if requestSize == 0 || requestSize > maxWorkerRequestBytes {
		return stampFailure("decode worker request",
			fmt.Errorf("request size %d is outside 1..%d bytes", requestSize, maxWorkerRequestBytes))
	}
	requestBytes := make([]byte, requestSize)
	if _, err := io.ReadFull(input, requestBytes); err != nil {
		return stampFailure("decode worker request", err)
	}
	var request workerRequest
	if err := json.Unmarshal(requestBytes, &request, json.RejectUnknownMembers(true)); err != nil {
		return stampFailure("decode worker request", err)
	}
	if request.Sources < 1 || request.Sources > 100_000 {
		return stampFailure("decode worker request", fmt.Errorf("source count %d is outside 1..100000", request.Sources))
	}
	sources, err := readWorkerSources(input, request.Sources)
	if err != nil {
		return err
	}
	var staged bytes.Buffer
	var pageCount int
	switch request.Operation {
	case "stamp_selected":
		var result SelectedResult
		result, err = StampSelected(ctx, bytes.NewReader(sources[0]), request.Labels, request.Recipe, &staged)
		pageCount = result.PageCount
	case "select":
		var result SelectedResult
		result, err = SelectPages(ctx, bytes.NewReader(sources[0]), request.Pages, &staged)
		pageCount = result.PageCount
	case "combine":
		var result Result
		result, err = CombineStamped(ctx, sources, request.Labels, &staged)
		pageCount = result.PageCount
	default:
		return stampFailure("decode worker request", fmt.Errorf("unknown operation %q", request.Operation))
	}
	if err != nil {
		return err
	}
	if err := binary.Write(output, binary.BigEndian, uint64(pageCount)); err != nil { //nolint:gosec // verified page counts are positive.
		return stampFailure("write worker result", err)
	}
	return writeSelectedOutput(output, staged.Bytes())
}

func readWorkerSources(input io.Reader, count int) ([][]byte, error) {
	sources := make([][]byte, count)
	var total int64
	for index := range sources {
		var size uint64
		if err := binary.Read(input, binary.BigEndian, &size); err != nil {
			return nil, stampFailure("decode worker source", err)
		}
		if size == 0 || size > uint64(MaxOutputBytes) {
			return nil, stampFailure("decode worker source", fmt.Errorf("source size %d is outside 1..%d bytes", size, MaxOutputBytes))
		}
		total += int64(size)
		if total > MaxOutputBytes {
			return nil, stampFailure("decode worker sources", fmt.Errorf("sources exceed %d bytes", MaxOutputBytes))
		}
		sources[index] = make([]byte, size)
		if _, err := io.ReadFull(input, sources[index]); err != nil {
			return nil, stampFailure("decode worker source", err)
		}
	}
	return sources, nil
}
