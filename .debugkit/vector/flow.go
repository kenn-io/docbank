package vector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"
)

type fillOptions[K comparable] struct {
	scanBatch               int
	documentLimit           int
	split                   SplitOptions
	batch                   batchOptions
	concurrency             int
	prepare                 func(context.Context, Pending[K]) ([]PreparedChunk, error)
	onProgress              func(FillProgress[K])
	onEncodeError           func(doc K, err error) bool
	shouldIsolateBatchError func(error) bool
	isolationSet            bool
}

// FillOption configures a Fill call.
type FillOption[K comparable] func(*fillOptions[K])

// WithFillScanBatch sets the number of pending documents fetched per scan.
// Values less than or equal to zero use 128.
func WithFillScanBatch[K comparable](size int) FillOption[K] {
	return func(o *fillOptions[K]) { o.scanBatch = size }
}

// WithFillSplit controls how each document's content is windowed into chunks.
func WithFillSplit[K comparable](options SplitOptions) FillOption[K] {
	return func(o *fillOptions[K]) { o.split = options }
}

// DefaultFillBatchSize is the most chunks Fill packs into one encode call
// when WithBatchSize is not positive. 32 is the largest request common
// embedding servers accept by default.
const DefaultFillBatchSize = 32

// DefaultFillBatchTokens is the estimated token total Fill packs into one
// encode call when WithBatchTokenBudget is not set. It stays under the
// smallest per-request token caps that hosted embedding providers publish.
// Fill estimates tokens conservatively: an ASCII rune counts as a quarter
// token and any other rune as one, so CJK text is not undercounted. A chunk
// whose estimate alone exceeds the budget is sent by itself.
const DefaultFillBatchTokens = 16384

// WithFillBatch controls how chunks are grouped into encode calls. Fill packs
// chunks across documents within each scan page. A call holds at most
// DefaultFillBatchSize chunks, or the positive WithBatchSize, and at most
// DefaultFillBatchTokens estimated tokens unless WithBatchTokenBudget sets
// the caller's own bound.
func WithFillBatch[K comparable](options ...BatchOption) FillOption[K] {
	return func(o *fillOptions[K]) { o.batch = applyBatchOptions(options) }
}

// WithFillConcurrency bounds parallel fill work within each scan page. Values
// less than or equal to zero use one worker. SaveVectors and the error hooks
// stay serialized on the calling goroutine. When WithBatchSize is positive,
// this limit composes with WithBatchConcurrency; their product bounds the
// concurrent encode calls.
func WithFillConcurrency[K comparable](concurrency int) FillOption[K] {
	return func(o *fillOptions[K]) { o.concurrency = concurrency }
}

// WithFillEncodeError handles a document that fails to encode. Returning true
// skips and stamp-saves that document so later Fill calls do not retry it.
// Returning false aborts Fill, which is the right default for transient
// failures. Omitting this option also aborts on the first encode error.
func WithFillEncodeError[K comparable](handler func(doc K, err error) bool) FillOption[K] {
	return func(o *fillOptions[K]) { o.onEncodeError = handler }
}

// WithFillBatchErrorIsolation decides whether a failed shared encode call is
// worth diagnosing at document-slice granularity. Returning true permits
// diagnosis only; WithFillEncodeError still decides whether to skip an
// attributed document. Fill bypasses this handler for single-document calls,
// context errors, and errors with an exact *InvalidVectorError position.
// A nil classify diagnoses nothing. Without this option, Fill diagnoses every
// shared-call error when WithFillEncodeError is set, so one bad document can
// still be skipped, and diagnoses none otherwise, because the first
// attributed error would abort.
func WithFillBatchErrorIsolation[K comparable](classify func(error) bool) FillOption[K] {
	return func(o *fillOptions[K]) {
		o.shouldIsolateBatchError = classify
		o.isolationSet = true
	}
}

// WithFillDocumentLimit stops Fill after it has started n pending documents.
// Documents already started are finished, including a document whose save
// returns ErrStale. Values less than or equal to zero do not limit the run.
// A later Fill continues with whatever is still pending.
func WithFillDocumentLimit[K comparable](n int) FillOption[K] {
	return func(o *fillOptions[K]) { o.documentLimit = n }
}

// WithFillPrepared supplies the chunks for each pending document and
// replaces WithFillSplit for that Fill call. The callback receives Fill's
// context. Fill does not call it for a later document in the page when that
// context is already cancelled, and it does not encode or stamp the page.
// An empty chunk list is a stamp-only save; return one to skip a document
// the callback cannot prepare. Chunk indexes must be non-negative and unique
// within the document. An error, including a bad index, aborts the fill before
// any document in the current page is encoded or stamped. Span and Truncated
// are reported through WithFillProgress and are not stored.
func WithFillPrepared[K comparable](prepare func(context.Context, Pending[K]) ([]PreparedChunk, error)) FillOption[K] {
	return func(o *fillOptions[K]) { o.prepare = prepare }
}

// FillProgress is one document Fill has stamped. Vectors is zero for a
// stamp-only save. Chunks are the inputs that were prepared for the
// document, including a caller-supplied source span when present.
type FillProgress[K comparable] struct {
	Doc     K
	Vectors int
	Chunks  []PreparedChunk
}

// WithFillProgress reports each document after its save succeeds. A save
// that returns ErrStale does not report progress.
func WithFillProgress[K comparable](report func(FillProgress[K])) FillOption[K] {
	return func(o *fillOptions[K]) { o.onProgress = report }
}

func applyFillOptions[K comparable](options []FillOption[K]) fillOptions[K] {
	o := fillOptions[K]{}
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	return o
}

// FillStats reports what a Fill run embedded.
type FillStats struct {
	// Documents is the number of documents embedded and stamped.
	Documents int
	// Chunks is the total chunk vectors saved across Documents.
	Chunks int
	// Skipped counts documents stamped without vectors because
	// the WithFillEncodeError handler elected to skip them.
	Skipped int
	// Stale counts documents left pending because they changed between
	// scan and save (SaveVectors returned ErrStale). A later run retries
	// them at their new revision.
	Stale int
}

// Fill embeds every document that still needs the target generation: it
// scans the store for pending documents, splits and encodes each, and
// saves the resulting vectors, repeating until no documents remain. It is
// the generic scan-and-fill loop; the store decides what counts as
// pending and persists the results.
//
// A document that changes mid-run (ErrStale from SaveVectors) is left
// pending and not retried until the next Fill call, so an actively edited
// document cannot starve the loop.
//
// Chunks from adjacent documents in one scan page may share an encode call.
// Errors with exact document attribution go directly to the
// WithFillEncodeError handler. Other shared-call errors are diagnosed at
// document-slice granularity as WithFillBatchErrorIsolation describes.
// WithFillEncodeError remains the sole authority for skip-stamping an
// attributed document.
//
// WithFillPrepared replaces rune-window splitting for the call. Its callback
// receives the Fill context, and a cancelled context stops the page before
// later documents are prepared. WithFillDocumentLimit bounds how many pending
// documents the call starts. WithFillProgress reports each document after its
// save succeeds.
func Fill[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	options ...FillOption[K],
) (FillStats, error) {
	o := applyFillOptions(options)
	if err := ctx.Err(); err != nil {
		return FillStats{}, err
	}
	if err := o.batch.validate(); err != nil {
		return FillStats{}, err
	}
	if o.batch.batchSize <= 0 {
		o.batch.batchSize = DefaultFillBatchSize
	}
	if !o.isolationSet && o.onEncodeError != nil {
		o.shouldIsolateBatchError = func(error) bool { return true }
	}
	scanBatch := o.scanBatch
	if scanBatch <= 0 {
		scanBatch = 128
	}

	var stats FillStats
	stale := make(map[K]struct{})
	started := 0
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if o.documentLimit > 0 && started >= o.documentLimit {
			return stats, nil
		}
		fetch := scanBatch
		if o.documentLimit > 0 && o.documentLimit-started < fetch {
			fetch = o.documentLimit - started
		}
		// Stale documents remain pending and occupy scan slots; widening
		// the limit keeps fresh documents visible past them.
		pending, err := store.PendingForGeneration(ctx, gen, fetch+len(stale))
		if err != nil {
			return stats, fmt.Errorf("scan pending: %w", err)
		}

		docs := make([]Pending[K], 0, len(pending))
		for _, p := range pending {
			if _, ok := stale[p.Doc]; !ok {
				docs = append(docs, p)
			}
		}
		if o.documentLimit > 0 && len(docs) > o.documentLimit-started {
			docs = docs[:o.documentLimit-started]
		}
		if len(docs) == 0 {
			// Nothing left, or only stale documents remain pending; either
			// way this run is done.
			return stats, nil
		}
		started += len(docs)
		if err := fillPage(ctx, store, gen, enc, o, docs, stale, &stats); err != nil {
			return stats, err
		}
	}
}

// fillEncoded carries one document's encode outcome from a worker to the
// collecting goroutine.
type fillEncoded[K comparable] struct {
	doc      Pending[K]
	chunks   []Chunk
	prepared []PreparedChunk
	vectors  []Vector
	err      error
	skip     bool
}

type preparedDocument[K comparable] struct {
	pending  Pending[K]
	chunks   []Chunk
	prepared []PreparedChunk
}

func prepareFillChunks[K comparable](ctx context.Context, pending Pending[K], o fillOptions[K]) ([]Chunk, []PreparedChunk, error) {
	if o.prepare != nil {
		prepared, err := o.prepare(ctx, pending)
		if err != nil {
			return nil, nil, err
		}
		seen := make(map[int]struct{}, len(prepared))
		for _, chunk := range prepared {
			if chunk.Index < 0 {
				return nil, nil, fmt.Errorf("prepared chunk index %d is negative", chunk.Index)
			}
			if _, ok := seen[chunk.Index]; ok {
				return nil, nil, fmt.Errorf("prepared chunk index %d repeats", chunk.Index)
			}
			seen[chunk.Index] = struct{}{}
		}
		chunks := make([]Chunk, len(prepared))
		for i, chunk := range prepared {
			chunks[i] = Chunk{Index: chunk.Index, Text: chunk.Text}
		}
		return chunks, prepared, nil
	}
	chunks := Split(pending.Content, o.split)
	prepared := make([]PreparedChunk, len(chunks))
	for i, chunk := range chunks {
		prepared[i] = PreparedChunk{Index: chunk.Index, Text: chunk.Text}
	}
	return chunks, prepared, nil
}

type fillDocumentState[K comparable] struct {
	encoded   fillEncoded[K]
	remaining int
	failed    bool
	saved     bool
}

type fillChunkRef struct {
	doc   int
	chunk int
	value Chunk
}

type fillBatchResult struct {
	refs    []fillChunkRef
	vectors []Vector
	err     error
}

// fillPage embeds one scan page. A positive batch size packs fixed-size chunk
// batches across documents, then scatters vectors back before saving each
// document independently. The legacy per-document path remains in use when
// the batch size is unset, preserving its unbounded-per-document call shape.
func fillPage[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	prepared := make([]preparedDocument[K], len(docs))
	for i, doc := range docs {
		// Do not call the prepare callback for a later document once Fill's
		// context is cancelled. Leave this page unencoded and unstamped.
		if o.prepare != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		chunks, chunksPrepared, err := prepareFillChunks(ctx, doc, o)
		if err != nil {
			return fmt.Errorf("prepare document %v: %w", doc.Doc, err)
		}
		prepared[i] = preparedDocument[K]{pending: doc, chunks: chunks, prepared: chunksPrepared}
	}
	if o.batch.batchSize <= 0 || enc == nil {
		return fillPageByDocument(ctx, store, gen, enc, o, prepared, stale, stats)
	}
	return fillPageAcrossDocuments(ctx, store, gen, enc, o, prepared, stale, stats)
}

func fillPageAcrossDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []preparedDocument[K], stale map[K]struct{}, stats *FillStats,
) error {
	documentStates := make([]fillDocumentState[K], len(docs))
	var refs []fillChunkRef
	for doc, pending := range docs {
		documentStates[doc] = fillDocumentState[K]{
			encoded: fillEncoded[K]{
				doc:      pending.pending,
				chunks:   pending.chunks,
				prepared: pending.prepared,
				vectors:  make([]Vector, len(pending.chunks)),
			},
			remaining: len(pending.chunks),
		}
		for chunk, value := range pending.chunks {
			refs = append(refs, fillChunkRef{doc: doc, chunk: chunk, value: value})
		}
	}

	if len(refs) == 0 {
		return saveReadyDocuments(ctx, store, gen, o, documentStates, true, stale, stats)
	}

	batchSize := o.batch.effectiveBatchSize(len(refs))
	orderedSaves := o.concurrency <= 1
	if err := saveReadyDocuments(ctx, store, gen, o, documentStates, orderedSaves, stale, stats); err != nil {
		return err
	}

	batchConcurrency := max(o.batch.concurrency, 1)
	tokenBudget := 0
	if !o.batch.tokenBudgetSet {
		tokenBudget = DefaultFillBatchTokens
	}
	batches := splitFillRefs(refs, batchSize, tokenBudget)
	encode := func(workCtx context.Context, batch []fillChunkRef) fillBatchResult {
		return encodeFillBatch(workCtx, enc, batch)
	}
	var err error
	if orderedSaves {
		// A sequential fill completes and saves one bounded window before
		// starting another, so a save failure cannot launch later work.
		for start := 0; start < len(batches); start += batchConcurrency {
			window := batches[start:min(start+batchConcurrency, len(batches))]
			active := make([][]fillChunkRef, 0, len(window))
			for _, batch := range window {
				batch = activeFillRefs(batch, documentStates)
				if len(batch) > 0 {
					active = append(active, batch)
				}
			}
			err = runFillJobs(ctx, batchConcurrency, active, encode,
				func(result fillBatchResult) bool { return result.err != nil },
				func(batch fillBatchResult) error {
					return applyFillBatch(ctx, store, gen, o, enc, batch, documentStates,
						true, stale, stats)
				})
			if err != nil {
				break
			}
		}
	} else {
		// Schedule individual batches so a completed document can be saved
		// without waiting for an unrelated slow batch in the same window.
		// Already-dispatched work may finish after another batch skips one of
		// its documents; applyFillBatch discards those results without racing
		// workers against the serialized document state.
		workers := len(batches)
		// Compute the worker and batch concurrency product without
		// overflowing the product.
		if o.concurrency <= len(batches)/batchConcurrency {
			workers = o.concurrency * batchConcurrency
		}
		err = runFillJobs(ctx, workers, batches, encode,
			func(result fillBatchResult) bool { return result.err != nil },
			func(batch fillBatchResult) error {
				return applyFillBatch(ctx, store, gen, o, enc, batch, documentStates,
					false, stale, stats)
			})
	}
	if err != nil {
		return err
	}
	if incomplete := incompleteFillDocuments(documentStates); incomplete > 0 {
		return fmt.Errorf("fill page: %d documents were not completed", incomplete)
	}
	return nil
}

// splitFillRefs cuts refs into encode calls of at most size chunks. A
// positive tokenBudget also ends a call before its estimated tokens would
// exceed the budget; a single chunk over the budget forms its own call.
func splitFillRefs(refs []fillChunkRef, size, tokenBudget int) [][]fillChunkRef {
	parts := make([][]fillChunkRef, 0, 1+(len(refs)-1)/size)
	start, tokens := 0, 0
	for i, ref := range refs {
		estimate := 0
		if tokenBudget > 0 {
			estimate = estimateTokens(ref.value.Text)
		}
		full := i-start >= size || (tokenBudget > 0 && i > start && tokens+estimate > tokenBudget)
		if full {
			parts = append(parts, refs[start:i])
			start, tokens = i, 0
		}
		tokens += estimate
	}
	if start < len(refs) {
		parts = append(parts, refs[start:])
	}
	return parts
}

// estimateTokens is a conservative token count for batching. English-like
// text averages about four characters per token; other scripts, such as CJK,
// average about one token per character, so they count in full.
func estimateTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func activeFillRefs[K comparable](refs []fillChunkRef, states []fillDocumentState[K]) []fillChunkRef {
	active := make([]fillChunkRef, 0, len(refs))
	for _, ref := range refs {
		if !states[ref.doc].failed && !states[ref.doc].saved {
			active = append(active, ref)
		}
	}
	return active
}

func encodeFillBatch(ctx context.Context, enc EncodeFunc, refs []fillChunkRef) fillBatchResult {
	chunks := make([]Chunk, len(refs))
	for i, ref := range refs {
		chunks[i] = ref.value
	}
	vectors, err := EncodeBatched(ctx, enc, chunks)
	return fillBatchResult{refs: refs, vectors: vectors, err: err}
}

func offsetInvalidVectorError(err error, chunkOffset int) error {
	invalid, ok := errors.AsType[*InvalidVectorError](err)
	if !ok {
		return err
	}
	translated := *invalid
	translated.Chunk += chunkOffset
	return fmt.Errorf("encode document chunks at %d: %w", chunkOffset, errors.Join(&translated, err))
}

func applyFillVectors[K comparable](refs []fillChunkRef, vectors []Vector, states []fillDocumentState[K]) {
	for i, ref := range refs {
		state := &states[ref.doc]
		if state.saved || state.failed {
			continue
		}
		state.encoded.vectors[ref.chunk] = vectors[i]
		state.remaining--
	}
}

func fillRefsContainFailedDocument[K comparable](refs []fillChunkRef, states []fillDocumentState[K]) bool {
	for _, ref := range refs {
		if states[ref.doc].failed {
			return true
		}
	}
	return false
}

func splitFillRefsByDocument(refs []fillChunkRef) [][]fillChunkRef {
	var slices [][]fillChunkRef
	for start := 0; start < len(refs); {
		end := start + 1
		for end < len(refs) && refs[end].doc == refs[start].doc {
			end++
		}
		slices = append(slices, refs[start:end])
		start = end
	}
	return slices
}

func decideFillDocumentError[K comparable](
	ctx context.Context, o fillOptions[K], states []fillDocumentState[K], doc int, err error,
) error {
	state := &states[doc]
	if state.saved || state.failed {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("encode document %v: %w", state.encoded.doc.Doc, err)
	}
	if o.onEncodeError == nil || !o.onEncodeError(state.encoded.doc.Doc, err) {
		return fmt.Errorf("encode document %v: %w", state.encoded.doc.Doc, err)
	}
	state.encoded.err = err
	state.encoded.skip = true
	state.remaining = 0
	state.failed = true
	return nil
}

func probeFillDocumentSlices[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o fillOptions[K],
	refs []fillChunkRef, states []fillDocumentState[K], orderedSaves bool,
	stale map[K]struct{}, stats *FillStats,
) (bool, error) {
	failed := false
	for _, documentSlice := range splitFillRefsByDocument(refs) {
		active := activeFillRefs(documentSlice, states)
		if len(active) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		probe := encodeFillBatch(ctx, enc, active)
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		if probe.err != nil {
			if errors.Is(probe.err, context.Canceled) || errors.Is(probe.err, context.DeadlineExceeded) {
				return failed, fmt.Errorf("encode document %v: %w",
					states[active[0].doc].encoded.doc.Doc, probe.err)
			}
			failed = true
			err := offsetInvalidVectorError(probe.err, active[0].chunk)
			if err := decideFillDocumentError(ctx, o, states, active[0].doc, err); err != nil {
				return failed, err
			}
		} else {
			applyFillVectors(active, probe.vectors, states)
		}
		if err := saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats); err != nil {
			return failed, err
		}
	}
	return failed, nil
}

func applyAttributedInvalidFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o fillOptions[K],
	batch fillBatchResult, invalid *InvalidVectorError, states []fillDocumentState[K],
	orderedSaves bool, stale map[K]struct{}, stats *FillStats,
) error {
	if invalid.Chunk < 0 || invalid.Chunk >= len(batch.refs) {
		return fillBatchContextError(fmt.Errorf(
			"invalid vector chunk %d outside batch of %d chunks: %w",
			invalid.Chunk, len(batch.refs), batch.err), batch.refs, states)
	}

	failingRef := batch.refs[invalid.Chunk]
	failingState := &states[failingRef.doc]
	if !failingState.saved && !failingState.failed {
		err := offsetInvalidVectorError(batch.err, failingRef.chunk-invalid.Chunk)
		if err := decideFillDocumentError(ctx, o, states, failingRef.doc, err); err != nil {
			return err
		}
		if err := saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats); err != nil {
			return err
		}
	}

	recovery := make([]fillChunkRef, 0, len(batch.refs))
	for _, ref := range batch.refs {
		if ref.doc != failingRef.doc && !states[ref.doc].saved && !states[ref.doc].failed {
			recovery = append(recovery, ref)
		}
	}
	_, err := probeFillDocumentSlices(ctx, store, gen, enc, o, recovery, states,
		orderedSaves, stale, stats)
	return err
}

func applyFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K], enc EncodeFunc,
	batch fillBatchResult, states []fillDocumentState[K], orderedSaves bool,
	stale map[K]struct{}, stats *FillStats,
) error {
	if batch.err == nil {
		applyFillVectors(batch.refs, batch.vectors, states)
		return saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(batch.err, context.Canceled) || errors.Is(batch.err, context.DeadlineExceeded) {
		return fillBatchContextError(batch.err, batch.refs, states)
	}
	if invalid, ok := errors.AsType[*InvalidVectorError](batch.err); ok {
		return applyAttributedInvalidFillBatch(ctx, store, gen, enc, o, batch, invalid,
			states, orderedSaves, stale, stats)
	}

	if batch.refs[0].doc == batch.refs[len(batch.refs)-1].doc {
		active := activeFillRefs(batch.refs, states)
		if len(active) == 0 {
			return nil
		}
		err := offsetInvalidVectorError(batch.err, active[0].chunk)
		if err := decideFillDocumentError(ctx, o, states, active[0].doc, err); err != nil {
			return err
		}
		return saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats)
	}

	explained := fillRefsContainFailedDocument(batch.refs, states)
	active := activeFillRefs(batch.refs, states)
	if len(active) == 0 {
		return nil
	}
	if o.shouldIsolateBatchError == nil || !o.shouldIsolateBatchError(batch.err) {
		return fillBatchContextError(batch.err, active, states)
	}
	failed, err := probeFillDocumentSlices(ctx, store, gen, enc, o, active, states,
		orderedSaves, stale, stats)
	if err != nil {
		return err
	}
	if !failed && !explained {
		return fillBatchContextError(fmt.Errorf(
			"cross-document batch failed but no document failed in isolation: %w", batch.err),
			active, states)
	}
	return nil
}

func fillBatchContextError[K comparable](
	err error, refs []fillChunkRef, states []fillDocumentState[K],
) error {
	for _, ref := range refs {
		if !states[ref.doc].saved {
			return fmt.Errorf("encode batch beginning with document %v: %w",
				states[ref.doc].encoded.doc.Doc, err)
		}
	}
	return err
}

func saveReadyDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K],
	states []fillDocumentState[K], ordered bool, stale map[K]struct{}, stats *FillStats,
) error {
	for i := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := &states[i]
		if state.saved {
			continue
		}
		if state.remaining != 0 {
			if ordered {
				break
			}
			continue
		}
		if err := saveEncoded(ctx, store, gen, o, state.encoded, stale, stats); err != nil {
			return err
		}
		state.saved = true
	}
	return nil
}

func incompleteFillDocuments[K comparable](states []fillDocumentState[K]) int {
	incomplete := 0
	for _, state := range states {
		if !state.saved {
			incomplete++
		}
	}
	return incomplete
}

type fillJobResult[R any] struct {
	value     R
	collected chan struct{}
}

func runFillJobs[J, R any](
	ctx context.Context,
	workers int,
	jobs []J,
	work func(context.Context, J) R,
	holdUntilCollected func(R) bool,
	collect func(R) error,
) error {
	workers = min(workers, len(jobs))
	if workers <= 1 {
		for _, job := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := collect(work(ctx, job)); err != nil {
				return err
			}
		}
		return nil
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobCh := make(chan J)
	results := make(chan fillJobResult[R])
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range jobCh {
				value := work(workCtx, job)
				var collected chan struct{}
				if holdUntilCollected != nil && holdUntilCollected(value) {
					collected = make(chan struct{})
				}
				select {
				case results <- fillJobResult[R]{value: value, collected: collected}:
				case <-workCtx.Done():
					return
				}
				if collected != nil {
					select {
					case <-collected:
					case <-workCtx.Done():
						return
					}
				}
			}
		})
	}
	go func() {
		defer close(jobCh)
		for _, job := range jobs {
			select {
			case jobCh <- job:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var firstErr error
	for result := range results {
		if firstErr == nil {
			if err := collect(result.value); err != nil {
				firstErr = err
				cancel()
			}
		}
		if result.collected != nil {
			close(result.collected)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// fillPageByDocument is the original per-document path used when the batch
// size is unset. Workers split and encode up to o.concurrency documents in parallel
// while the calling goroutine saves each result as it completes. The first
// save-side failure cancels in-flight encodes and is returned.
func fillPageByDocument[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []preparedDocument[K], stale map[K]struct{}, stats *FillStats,
) error {
	return runFillJobs(ctx, o.concurrency, docs,
		func(workCtx context.Context, doc preparedDocument[K]) fillEncoded[K] {
			vectors, err := encodeBatched(workCtx, enc, doc.chunks, o.batch)
			return fillEncoded[K]{
				doc: doc.pending, chunks: doc.chunks, prepared: doc.prepared,
				vectors: vectors, err: err,
			}
		},
		nil,
		func(result fillEncoded[K]) error {
			return saveEncoded(ctx, store, gen, o, result, stale, stats)
		})
}

// saveEncoded applies one document's encode outcome: it consults the encode
// error handler for failures, stamps skips, saves vectors, and records
// stale revisions, updating stats to match. It runs on Fill's calling
// goroutine only.
func saveEncoded[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K],
	r fillEncoded[K], stale map[K]struct{}, stats *FillStats,
) error {
	skipped := r.skip
	if r.err != nil && !skipped {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		if o.onEncodeError == nil || !o.onEncodeError(r.doc.Doc, r.err) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		skipped = true
	}

	var cvs []ChunkVector
	if !skipped {
		cvs = make([]ChunkVector, len(r.chunks))
		for i, c := range r.chunks {
			cvs[i] = ChunkVector{ChunkIndex: c.Index, Vector: r.vectors[i]}
		}
	}
	if err := store.SaveVectors(ctx, gen, r.doc.Doc, r.doc.Revision, cvs); err != nil {
		if errors.Is(err, ErrStale) {
			stale[r.doc.Doc] = struct{}{}
			stats.Stale++
			return nil
		}
		return fmt.Errorf("save document %v: %w", r.doc.Doc, err)
	}
	if o.onProgress != nil {
		reported := 0
		if !skipped {
			reported = len(cvs)
		}
		o.onProgress(FillProgress[K]{Doc: r.doc.Doc, Vectors: reported, Chunks: r.prepared})
	}
	if skipped {
		stats.Skipped++
		return nil
	}
	stats.Documents++
	stats.Chunks += len(cvs)
	return nil
}

// SearchOptions configures Search.
type SearchOptions struct {
	// PerGeneration caps how many hits are fetched from each generation
	// before merging. Values <= 0 use 50.
	PerGeneration int
	// Merge configures how per-generation results are combined.
	Merge MergeOptions
}

// Search embeds queryText once per live generation (each may use a
// different model), queries each generation, rolls the chunk hits up to
// documents, and merges the per-generation results into one ranking.
// encFor maps a generation to the encoder for that generation's model.
func Search[K, G comparable](
	ctx context.Context,
	store Store[K, G],
	queryText string,
	encFor func(gen G) EncodeFunc,
	o SearchOptions,
) ([]Hit[K], error) {
	perGen := o.PerGeneration
	if perGen <= 0 {
		perGen = 50
	}

	gens, err := store.LiveGenerations(ctx)
	if err != nil {
		return nil, fmt.Errorf("live generations: %w", err)
	}

	lists := make([][]Hit[K], 0, len(gens))
	for _, gen := range gens {
		enc := encFor(gen)
		if enc == nil {
			return nil, fmt.Errorf("no encoder for generation %v", gen)
		}
		vectors, err := EncodeBatched(ctx, enc, []Chunk{{Index: 0, Text: queryText}})
		if err != nil {
			return nil, fmt.Errorf("embed query for generation %v: %w", gen, err)
		}
		if len(vectors) != 1 {
			return nil, fmt.Errorf("embed query for generation %v returned %d vectors, want 1", gen, len(vectors))
		}
		hits, err := store.QueryGeneration(ctx, gen, vectors[0], perGen)
		if err != nil {
			return nil, fmt.Errorf("query generation %v: %w", gen, err)
		}
		rolled, err := RollupByDocument(hits)
		if err != nil {
			return nil, fmt.Errorf("rank generation %v: %w", gen, err)
		}
		lists = append(lists, rolled)
	}
	merged, err := Merge(lists, o.Merge)
	if err != nil {
		return nil, fmt.Errorf("merge generations: %w", err)
	}
	return merged, nil
}
