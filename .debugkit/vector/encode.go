package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
)

// Vector is a single embedding.
type Vector []float32

// ErrEmptyEmbeddingInput reports that a text passed for embedding carries
// nothing to embed: it is empty, or every rune is whitespace, invisible
// formatting, or a control character. EncodeBatched detects it before invoking
// the encoder, so callers can safely recognize it with errors.Is without
// depending on a provider's validation response.
var ErrEmptyEmbeddingInput = errors.New(
	"embedding input is empty or contains only whitespace and invisible formatting")

// InvalidVectorError reports an encoder output vector that has the expected
// count but cannot participate in cosine distance: a non-finite component or
// a zero norm. Chunk is the index within the chunks passed to EncodeBatched;
// Component is the offending component, or -1 for a zero-norm vector.
type InvalidVectorError struct {
	Chunk     int
	Component int
	Reason    string
}

func (e *InvalidVectorError) Error() string {
	if e.Component >= 0 {
		return fmt.Sprintf("invalid vector for chunk %d component %d: %s", e.Chunk, e.Component, e.Reason)
	}
	return fmt.Sprintf("invalid vector for chunk %d: %s", e.Chunk, e.Reason)
}

// EncodeFunc turns a batch of texts into one vector each, in the same
// order. Implementations own the model or API client and any retry or
// backoff policy, since retryability is provider-specific.
type EncodeFunc func(ctx context.Context, texts []string) ([][]float32, error)

type batchOptions struct {
	batchSize            int
	concurrency          int
	maxBatchTokens       int
	inputTokenUpperBound int
	tokenBudgetSet       bool
}

// BatchOption configures batching for EncodeBatched or Fill.
type BatchOption func(*batchOptions)

// WithBatchSize limits the number of chunks in one EncodeFunc call. Values
// less than or equal to zero send every available chunk in one call.
func WithBatchSize(size int) BatchOption {
	return func(o *batchOptions) {
		o.batchSize = size
	}
}

// WithBatchConcurrency limits concurrent EncodeFunc calls. Values less than
// or equal to zero use one call at a time.
func WithBatchConcurrency(concurrency int) BatchOption {
	return func(o *batchOptions) {
		o.concurrency = concurrency
	}
}

// WithBatchTokenBudget keeps an EncodeFunc call within an encoder's aggregate
// input-token limit when every input is known to contain at most
// inputTokenUpperBound tokens. Use it when the encoder enforces a combined
// request limit that a count-only BatchSize cannot represent.
//
// The option caps the batch at maxBatchTokens / inputTokenUpperBound inputs,
// or fewer when BatchSize is smaller. The caller must choose a conservative
// per-input bound from its model and chunking rules. EncodeBatched does not
// count tokens, so a conservative bound can intentionally leave some request
// capacity unused. Fill uses this bound instead of its own token estimate
// when the option is set. Both values must be positive, and one input must fit within
// maxBatchTokens.
func WithBatchTokenBudget(maxBatchTokens, inputTokenUpperBound int) BatchOption {
	return func(o *batchOptions) {
		o.maxBatchTokens = maxBatchTokens
		o.inputTokenUpperBound = inputTokenUpperBound
		o.tokenBudgetSet = true
	}
}

// EncodeBatched splits chunks into batches, invokes enc with bounded
// concurrency, and returns one Vector per input chunk in input order. It
// stops launching work at the first error or when ctx is cancelled, and
// reports the first error encountered.
//
// Encoder output that has the right count but cannot participate in cosine
// distance — a non-finite component or a zero-norm vector — is rejected with
// an error wrapping *InvalidVectorError, so faulty endpoint output never
// reaches a Store. Blank chunk text is rejected with an error wrapping
// ErrEmptyEmbeddingInput before any EncodeFunc call.
func EncodeBatched(
	ctx context.Context, enc EncodeFunc, chunks []Chunk, options ...BatchOption,
) ([]Vector, error) {
	return encodeBatched(ctx, enc, chunks, applyBatchOptions(options))
}

// EncodeOne encodes a single text, typically a search query, with the same
// blank-input and invalid-vector checks EncodeBatched applies.
func EncodeOne(ctx context.Context, enc EncodeFunc, text string, options ...BatchOption) (Vector, error) {
	vectors, err := EncodeBatched(ctx, enc, []Chunk{{Index: 0, Text: text}}, options...)
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("encoder returned %d vectors for one text", len(vectors))
	}
	return vectors[0], nil
}

func encodeBatched(
	ctx context.Context, enc EncodeFunc, chunks []Chunk, o batchOptions,
) ([]Vector, error) {
	if enc == nil {
		return nil, errors.New("encode func is nil")
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	for i, chunk := range chunks {
		if blank(chunk.Text) {
			return nil, fmt.Errorf("encode chunk %d: %w", i, ErrEmptyEmbeddingInput)
		}
	}

	batchSize := o.effectiveBatchSize(len(chunks))
	concurrency := o.concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	out := make([]Vector, len(chunks))
	sem := make(chan struct{}, concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

launch:
	for start := 0; start < len(chunks); start += batchSize {
		if ctx.Err() != nil {
			setErr(ctx.Err())
			break
		}
		if failed() {
			break
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			setErr(ctx.Err())
			break launch
		}
		if ctx.Err() != nil {
			<-sem
			setErr(ctx.Err())
			break
		}
		if failed() {
			<-sem
			break
		}

		end := min(start+batchSize, len(chunks))
		texts := make([]string, end-start)
		for i, c := range chunks[start:end] {
			texts[i] = c.Text
		}

		wg.Add(1)
		go func(start int, texts []string) {
			defer wg.Done()
			defer func() { <-sem }()

			vecs, err := enc(ctx, texts)
			if err != nil {
				setErr(fmt.Errorf("encode batch at %d: %w", start, err))
				return
			}
			if len(vecs) != len(texts) {
				setErr(fmt.Errorf("encode batch at %d: got %d vectors for %d texts", start, len(vecs), len(texts)))
				return
			}
			// Each batch owns a disjoint index range, so writes to out
			// never overlap across goroutines.
			for i, v := range vecs {
				if err := validateVector(v, start+i); err != nil {
					setErr(fmt.Errorf("encode batch at %d: %w", start, err))
					return
				}
				out[start+i] = Vector(v)
			}
		}(start, texts)
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func applyBatchOptions(options []BatchOption) batchOptions {
	o := batchOptions{}
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	return o
}

func (o batchOptions) validate() error {
	if !o.tokenBudgetSet {
		return nil
	}
	if o.maxBatchTokens <= 0 || o.inputTokenUpperBound <= 0 {
		return fmt.Errorf(
			"batch token budget and input token upper bound must be positive: got %d and %d",
			o.maxBatchTokens, o.inputTokenUpperBound)
	}
	if o.inputTokenUpperBound > o.maxBatchTokens {
		return fmt.Errorf(
			"input token upper bound %d exceeds batch token budget %d",
			o.inputTokenUpperBound, o.maxBatchTokens)
	}
	return nil
}

func (o batchOptions) effectiveBatchSize(chunkCount int) int {
	batchSize := o.batchSize
	if batchSize <= 0 {
		batchSize = chunkCount
	}
	if !o.tokenBudgetSet {
		return batchSize
	}
	return min(batchSize, o.maxBatchTokens/o.inputTokenUpperBound)
}

// validateVector rejects vectors that would poison cosine distance: any
// non-finite component, or a vector whose norm is zero. chunk is the global
// chunk index reported in the error.
func validateVector(v []float32, chunk int) error {
	var squaredNorm float64
	for component, value := range v {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return &InvalidVectorError{Chunk: chunk, Component: component, Reason: "non-finite component"}
		}
		squaredNorm += f * f
	}
	if squaredNorm == 0 {
		return &InvalidVectorError{Chunk: chunk, Component: -1, Reason: "zero norm"}
	}
	return nil
}
