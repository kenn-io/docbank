package pdfproduction

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

//go:embed pdfium.wasm
var wasmBytes []byte

type pdfiumEngine struct {
	gate     chan struct{}
	recipe   redaction.Recipe
	cache    wazero.CompilationCache
	session  *backingLease
	scavenge func()
	closed   bool
}

// The worker contract allows one WASM session at a time. Keep exactly one
// backing allocation across engine lifetimes; dropping large Go allocations
// between sessions can leave their physical pages resident until a later GC.
var wasmBacking = newBackingManager(512 << 20)

// Exactly one embedded module is compiled. Keep its code-only cache for the
// process lifetime as well: retiring a large compilation for every engine can
// retain heap-system pages even after all runtime/document handles are closed.
// The exclusive backing lease serializes compilation and runtime access.
var wasmCode = wazero.NewCompilationCache()

type backingManager struct {
	mu       sync.Mutex
	token    chan struct{}
	capacity uint64
	buffer   []byte
	active   *backingLease
}

type backingLease struct {
	owner  *backingManager
	memory *linearMemoryPool
}

func newBackingManager(capacity uint64) *backingManager {
	return &backingManager{token: make(chan struct{}, 1), capacity: capacity}
}

func (m *backingManager) acquire(ctx context.Context, limit uint64) (*backingLease, error) {
	if limit == 0 || limit > m.capacity {
		return nil, errors.New("invalid WASM backing limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case m.token <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-m.token
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = &backingLease{owner: m, memory: &linearMemoryPool{limit: limit, capacity: m.capacity, buffer: m.buffer}}
	m.buffer = nil
	return m.active, nil
}

func (l *backingLease) release() {
	l.releaseAfter(nil)
}

func (l *backingLease) releaseAfter(cleanup func()) {
	m := l.owner
	m.mu.Lock()
	if m.active != l {
		m.mu.Unlock()
		return
	}
	m.buffer = l.memory.detach()
	m.active = nil
	m.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	<-m.token
}

// linearMemoryPool owns one fixed-capacity backing buffer across sequential page
// runtimes. Only an active lease may resize it; runtime teardown clears every
// previously accessible byte before the next page can acquire it. As with
// wazero's default allocator, borrowed views are invalid after Free.
type linearMemoryPool struct {
	mu       sync.Mutex
	limit    uint64
	capacity uint64
	buffer   []byte
	active   *fixedLinearMemory
	closed   bool
}

type fixedLinearMemory struct {
	owner     *linearMemoryPool
	highWater uint64
}

func (p *linearMemoryPool) acquire() (*fixedLinearMemory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.active != nil {
		return nil, errors.New("WASM linear memory is closed or already leased")
	}
	p.active = &fixedLinearMemory{owner: p}
	return p.active, nil
}

func (p *linearMemoryPool) close() { p.detach() }

func (p *linearMemoryPool) detach() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.active != nil {
		clear(p.buffer[:p.active.highWater])
		p.active = nil
	}
	buffer := p.buffer
	p.buffer = nil
	return buffer
}

func (m *fixedLinearMemory) Reallocate(size uint64) []byte {
	p := m.owner
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.active != m || size > p.limit {
		return nil
	}
	if p.buffer == nil {
		// Allocate only at instantiation, after compilation. Go initially maps
		// untouched capacity without making all of it resident. Growth never
		// allocates or copies a second backing buffer.
		p.buffer = make([]byte, max(p.limit, p.capacity))
	}
	m.highWater = max(m.highWater, size)
	return p.buffer[:size]
}

func (m *fixedLinearMemory) Free() {
	p := m.owner
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != m {
		return
	}
	if !p.closed {
		clear(p.buffer[:m.highWater])
	}
	p.active = nil
}

// NewPDFium acquires the worker's exclusive WASM backing lazily on first use.
// Close a rendering engine before starting another engine or VerifyFresh;
// concurrent sessions wait with their page context and deadline.
func NewPDFium(recipe redaction.Recipe) (Engine, error) {
	if err := validateRecipe(recipe); err != nil {
		return nil, err
	}
	if hashBytes(wasmBytes) != wasmSHA256 || recipe.RendererSHA256 != "" && recipe.RendererSHA256 != wasmSHA256 {
		return nil, errors.New("qualified PDFium WASM asset unavailable or hash mismatch")
	}
	if err := checkRSS(context.Background(), recipe.QualifiedPeakRSSBytes); err != nil {
		return nil, err
	}
	return &pdfiumEngine{gate: make(chan struct{}, 1), recipe: recipe, cache: wasmCode, scavenge: debug.FreeOSMemory}, nil
}

// VerifyFresh uses PDFium's independent parser, Unicode extraction, page-object
// enumeration, and rendering. Expected authority comes exclusively from input.
func VerifyFresh(ctx context.Context, reader io.ReaderAt, size int64, input VerificationInput) error {
	return verifyFresh(ctx, reader, size, input, defaultRecipe())
}

// VerifyFreshWithRecipe verifies final pixels at the same qualified resolution
// and limits used to produce them. The caller supplies private receipt authority.
func VerifyFreshWithRecipe(ctx context.Context, reader io.ReaderAt, size int64, input VerificationInput, recipe redaction.Recipe) error {
	return verifyFresh(ctx, reader, size, input, recipe)
}

func verifyFresh(ctx context.Context, reader io.ReaderAt, size int64, input VerificationInput, recipe redaction.Recipe) (result error) {
	if err := validateRecipe(recipe); err != nil {
		return err
	}
	if size > maxPDFFileBytes {
		return errors.New("PDF exceeds pinned WASM 32-bit file length limit")
	}
	n := len(input.Pages)
	if reader == nil || size <= 0 || size > maxPayloadBytes || n == 0 || n > maxPageCount || len(input.ResolvedSHA256) != n || len(input.EndorsementsSHA256) != n || len(input.LayoutSHA256) != n || input.NextText == nil || input.NextPageArtifact == nil || input.NextEndorsements == nil {
		return errors.New("invalid fresh PDF verification input")
	}
	ctx, stop, err := watchRSS(ctx, recipe.QualifiedPeakRSSBytes)
	if err != nil {
		return err
	}
	defer stop()
	defer func(operation context.Context) {
		if operation.Err() != nil {
			result = errors.Join(result, context.Cause(operation))
		}
	}(ctx)
	setupCtx, setupCancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
	defer setupCancel()
	snapshot, err := freezeFresh(setupCtx, reader, size, recipe.MaxStagingBytes)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, snapshot.Close()) }()
	reader, size = snapshot.file, snapshot.size
	index, err := parseFreshIndex(setupCtx, reader, size, n)
	setupCancel()
	if err != nil {
		return err
	}
	engine, err := NewPDFium(recipe)
	if err != nil {
		return err
	}
	defer engine.Close() //nolint:errcheck // All per-page runtimes are closed independently.
	e, ok := engine.(*pdfiumEngine)
	if !ok {
		return errors.New("unexpected PDFium engine implementation")
	}
	for i := range input.Pages {
		if err := verifyFreshPage(ctx, e, reader, size, index, i, input); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	if _, _, err := input.NextText(ctx); !errors.Is(err, io.EOF) {
		return errors.New("sanitized text evidence has extra pages or failed termination")
	}
	if _, err := input.NextPageArtifact(ctx); !errors.Is(err, io.EOF) {
		return errors.New("artifact evidence has extra pages or failed termination")
	}
	if _, _, err := input.NextEndorsements(ctx); !errors.Is(err, io.EOF) {
		return errors.New("endorsement evidence has extra pages or failed termination")
	}
	return ctx.Err()
}

func verifyFreshPage(ctx context.Context, e *pdfiumEngine, reader io.ReaderAt, size int64, index *freshIndex, i int, input VerificationInput) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	page := input.Pages[i]
	if page.Number != i+1 {
		return errors.New("verification pages are out of order")
	}
	artifact, err := input.NextPageArtifact(ctx)
	if err != nil {
		return fmt.Errorf("next verification artifact: %w", err)
	}
	if artifact.Page != page || artifact.ResolvedSHA256 != input.ResolvedSHA256[i] || artifact.EndorsementsSHA256 != input.EndorsementsSHA256[i] || artifact.LayoutSHA256 != input.LayoutSHA256[i] {
		return errors.New("verification artifact identity mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateArtifact(artifact, e.recipe); err != nil {
		return err
	}
	pageNumber, expectedText, err := input.NextText(ctx)
	if err != nil {
		return fmt.Errorf("next sanitized text: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pageNumber != i+1 || len(expectedText) > maxTextBytes {
		return errors.New("sanitized text is out of order or oversized")
	}
	var runText strings.Builder
	for _, r := range artifact.Runs {
		runText.WriteString(r.Text)
	}
	if runText.String() != string(expectedText) {
		return errors.New("sanitized text differs from staged runs")
	}
	layout, endorsements, err := input.NextEndorsements(ctx)
	if err != nil {
		return fmt.Errorf("next endorsement evidence: %w", err)
	}
	if err := preflightEndorsements(endorsements); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := canonical.Marshal(endorsements)
	if err != nil {
		return err
	}
	if layout != artifact.Layout || hashBytes(encoded) != input.EndorsementsSHA256[i] {
		return errors.New("endorsement or layout evidence mismatch")
	}
	parsed, err := index.page(ctx, i, artifact, e.recipe)
	if err != nil {
		return err
	}
	err = e.withInstanceContext(ctx, func(ctx context.Context, instance pdfium.Pdfium) (pageErr error) {
		doc, err := instance.OpenDocument(&requests.OpenDocument{FileReader: io.NewSectionReader(reader, 0, size), FileReaderSize: size})
		if err != nil {
			return fmt.Errorf("open fresh PDF: %w", err)
		}
		count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil {
			return fmt.Errorf("count fresh PDF pages: %w", err)
		}
		if count.PageCount != len(input.Pages) {
			return errors.New("fresh PDF page count mismatch")
		}
		ref := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}}
		size, err := instance.GetPageSize(&requests.GetPageSize{Page: ref})
		if err != nil {
			return fmt.Errorf("inspect fresh page size: %w", err)
		}
		if math.Abs(size.Width-physicalPoints(page.Width)) > 0.001 || math.Abs(size.Height-physicalPoints(page.Height)) > 0.001 {
			return errors.New("fresh PDF page dimensions differ from expected output frame")
		}
		if err := checkTextObjects(instance, ref, artifact, parsed.advances); err != nil {
			return err
		}
		rows, err := openPNGRows(ctx, artifact, e.recipe)
		if err != nil {
			return err
		}
		defer func() { pageErr = errors.Join(pageErr, rows.Close()) }()
		comparison := newFreshImageComparison(ctx, parsed.image, rows.width)
		defer comparison.z.Close() //nolint:errcheck // Preserve the processing error; finish checks successful completion.
		for top := 0; top < rows.height; top += 256 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := compareTile(instance, ref, rows, comparison, top, min(256, rows.height-top)); err != nil {
				return fmt.Errorf("verify final pixels on page %d: %w", i+1, err)
			}
		}
		return errors.Join(rows.Finish(), comparison.finish())
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}

func compareTile(instance pdfium.Pdfium, page requests.Page, rows *pngRows, comparison *freshImageComparison, top, height int) error {
	width, fullHeight := rows.width, rows.height
	bitmap, err := instance.FPDFBitmap_Create(&requests.FPDFBitmap_Create{Width: width, Height: height, Alpha: 1})
	if err != nil {
		return fmt.Errorf("allocate verification tile: %w", err)
	}
	defer instance.FPDFBitmap_Destroy(&requests.FPDFBitmap_Destroy{Bitmap: bitmap.Bitmap}) //nolint:errcheck // Per-page runtime cleanup also releases the bitmap.
	if _, err := instance.FPDFBitmap_FillRect(&requests.FPDFBitmap_FillRect{Bitmap: bitmap.Bitmap, Width: width, Height: height, Color: 0xffffffff}); err != nil {
		return fmt.Errorf("initialize verification tile: %w", err)
	}
	if _, err := instance.FPDF_RenderPageBitmap(&requests.FPDF_RenderPageBitmap{Bitmap: bitmap.Bitmap, Page: page, StartY: -top, SizeX: width, SizeY: fullHeight, Flags: enums.FPDF_RENDER_FLAG_REVERSE_BYTE_ORDER}); err != nil {
		return fmt.Errorf("render verification tile: %w", err)
	}
	buffer, err := instance.FPDFBitmap_GetBuffer(&requests.FPDFBitmap_GetBuffer{Bitmap: bitmap.Bitmap})
	if err != nil {
		return fmt.Errorf("read rendered verification tile: %w", err)
	}
	line, err := instance.FPDFBitmap_GetStride(&requests.FPDFBitmap_GetStride{Bitmap: bitmap.Bitmap})
	if err != nil {
		return fmt.Errorf("read verification tile stride: %w", err)
	}
	actual, actualStride := buffer.Buffer, line.Stride
	for y := range height {
		expected, err := rows.Next()
		if err != nil {
			return err
		}
		if err := comparison.row(expected); err != nil {
			return err
		}
		if !bytes.Equal(expected, actual[y*actualStride:y*actualStride+width*4]) {
			return errors.New("rendered pixels differ from staged page")
		}
	}
	return nil
}

func checkTextObjects(instance pdfium.Pdfium, page requests.Page, a PageArtifact, advances []float64) error {
	annotations, err := instance.FPDFPage_GetAnnotCount(&requests.FPDFPage_GetAnnotCount{Page: page})
	if err != nil {
		return fmt.Errorf("inspect fresh annotations: %w", err)
	}
	if annotations.Count != 0 {
		return errors.New("fresh PDF contains annotations")
	}
	objects, err := instance.FPDFPage_CountObjects(&requests.FPDFPage_CountObjects{Page: page})
	if err != nil {
		return fmt.Errorf("inspect fresh page objects: %w", err)
	}
	if objects.Count != 1+len(a.Runs)+len(a.Endorsements) {
		return errors.New("fresh PDF has extra or missing image/text objects")
	}
	textPage, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: page})
	if err != nil {
		return fmt.Errorf("load fresh text page: %w", err)
	}
	defer instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: textPage.TextPage}) //nolint:errcheck // Instance cleanup also closes text pages.
	for i := range objects.Count {
		obj, err := instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: i})
		if err != nil {
			return fmt.Errorf("get fresh page object: %w", err)
		}
		kind, err := instance.FPDFPageObj_GetType(&requests.FPDFPageObj_GetType{PageObject: obj.PageObject})
		if err != nil {
			return fmt.Errorf("inspect fresh object type: %w", err)
		}
		if i == 0 {
			if kind.Type != enums.FPDF_PAGEOBJ_IMAGE {
				return errors.New("fresh PDF first object is not its burned image")
			}
			continue
		}
		if kind.Type != enums.FPDF_PAGEOBJ_TEXT {
			return errors.New("fresh PDF contains an unexpected nontext object")
		}
		var text, role string
		var box redaction.Box
		var size int64
		if i <= len(a.Runs) {
			run := a.Runs[i-1]
			text, role, box = run.Text, "SanitizedSource", runPlacement(run, a.Layout.Source)
			size = run.FontSizeMilliPoints
		} else {
			e := a.Endorsements[i-1-len(a.Runs)]
			text, role, box = e.Text, "Endorsement", e.Box
			size = e.FontSizeMilliPoints
		}
		if err := checkTextObject(instance, obj.PageObject, textPage.TextPage, a.Page, text, role, box, size, advances[i-1]); err != nil {
			return err
		}
	}
	return nil
}

func checkTextObject(instance pdfium.Pdfium, obj references.FPDF_PAGEOBJECT, textPage references.FPDF_TEXTPAGE, page redaction.Page, text, role string, box redaction.Box, size int64, advance float64) error {
	decoded, err := instance.FPDFTextObj_GetText(&requests.FPDFTextObj_GetText{PageObject: obj, TextPage: textPage})
	if err != nil {
		return fmt.Errorf("decode fresh text object: %w", err)
	}
	// PDFium suppresses coincident duplicate text when building a text page.
	// The independent CMap/content decoder above verifies every occurrence,
	// including these empty extraction results; object counts/roles/placement
	// remain mandatory. Never substitute a PDF-provided expected string.
	// PDFium collapses repeated spaces; compare its text after collapsing each
	// Unicode whitespace sequence and trimming edges. The independent content
	// decoder above still requires exact UTF-8, including every LF and space.
	if decoded.Text != "" && strings.Join(strings.Fields(decoded.Text), " ") != strings.Join(strings.Fields(text), " ") {
		return errors.New("fresh PDF decoded Unicode text mismatch")
	}
	marks, err := instance.FPDFPageObj_CountMarks(&requests.FPDFPageObj_CountMarks{PageObject: obj})
	if err != nil {
		return fmt.Errorf("count fresh stream roles: %w", err)
	}
	if marks.Count != 1 {
		return errors.New("fresh PDF text has missing or extra stream roles")
	}
	mark, err := instance.FPDFPageObj_GetMark(&requests.FPDFPageObj_GetMark{PageObject: obj, Index: 0})
	if err != nil {
		return fmt.Errorf("read fresh stream role: %w", err)
	}
	name, err := instance.FPDFPageObjMark_GetName(&requests.FPDFPageObjMark_GetName{PageObjectMark: mark.Mark})
	if err != nil {
		return fmt.Errorf("decode fresh stream role: %w", err)
	}
	if name.Name != role {
		return errors.New("fresh text stream role mismatch")
	}
	matrix, err := instance.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: obj})
	if err != nil {
		return fmt.Errorf("read fresh text placement: %w", err)
	}
	m := matrix.Matrix
	xscale := float64(box.X1-box.X0) * 72 * 100 / (advance * float64(size))
	if math.Abs(float64(m.A)-xscale) > 0.00001 || m.B != 0 || m.C != 0 || m.D != 1 || math.Abs(float64(m.E)-physicalPoints(box.X0)) > 0.001 || math.Abs(float64(m.F)-physicalPoints(page.Height-box.Y1)) > 0.001 {
		return errors.New("fresh text placement differs from staged geometry")
	}
	fontSize, err := instance.FPDFTextObj_GetFontSize(&requests.FPDFTextObj_GetFontSize{PageObject: obj})
	if err != nil {
		return fmt.Errorf("read fresh font size: %w", err)
	}
	if math.Abs(float64(fontSize.FontSize)-float64(size)/1000) > 0.001 {
		return errors.New("fresh text size differs from staged geometry")
	}
	mode, err := instance.FPDFTextObj_GetTextRenderMode(&requests.FPDFTextObj_GetTextRenderMode{PageObject: obj})
	if err != nil {
		return fmt.Errorf("read fresh text render mode: %w", err)
	}
	if mode.TextRenderMode != 3 {
		return errors.New("fresh searchable text must be invisible over burned pixels")
	}
	return nil
}

func (e *pdfiumEngine) Close() error {
	e.gate <- struct{}{}
	defer func() { <-e.gate }()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.session != nil {
		// A closed engine is the worker lifecycle boundary. Return garbage from
		// the isolated runtime and page renderings to the OS before another job
		// is measured against the process-wide qualified RSS ceiling. The
		// cleared fixed WASM backing and compiled code remain reusable. Retain
		// the exclusive process-wide lease until scavenging is complete so the
		// next engine cannot allocate against memory that cleanup still owns.
		e.session.releaseAfter(e.scavenge)
		e.session = nil
	}
	return nil
}

// Each page gets a fresh runtime tied to its deadline. This bounds PDFium's
// document/font/image caches and ensures cancellation interrupts WASM execution.
// Only compiled code and a cleared backing allocation are reused; no document
// state survives a page.
func (e *pdfiumEngine) withInstance(ctx context.Context, fn func(pdfium.Pdfium) error) error {
	return e.withInstanceContext(ctx, func(_ context.Context, instance pdfium.Pdfium) error { return fn(instance) })
}

func (e *pdfiumEngine) withInstanceContext(ctx context.Context, fn func(context.Context, pdfium.Pdfium) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	ctx, stop, err := watchRSS(ctx, e.recipe.QualifiedPeakRSSBytes)
	if err != nil {
		return err
	}
	defer stop()
	select {
	case e.gate <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("acquire PDFium engine: %w", context.Cause(ctx))
	}
	defer func() { <-e.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed {
		return errors.New("PDFium engine is closed")
	}
	if e.session == nil {
		e.session, err = wasmBacking.acquire(ctx, uint64(e.recipe.WASMMemoryBytes)) //nolint:gosec // Recipe validation requires a positive, bounded limit.
		if err != nil {
			return fmt.Errorf("acquire exclusive WASM session: %w", err)
		}
	}
	lease, err := e.session.memory.acquire()
	if err != nil {
		return err
	}
	defer lease.Free() // Also release when initialization fails before runtime ownership.
	// Reserve the already bounded linear-memory capacity once. Wazero's
	// default growth path reallocates/copies the old buffer, which can exceed
	// the whole-worker RSS ceiling even while live WASM memory stays bounded.
	ctx = experimental.WithMemoryAllocator(ctx, experimental.MemoryAllocatorFunc(func(_ uint64, maximum uint64) experimental.LinearMemory {
		// Wazero validates and clamps this maximum to WithMemoryLimitPages
		// before invoking the allocator. The pinned asset itself declares 2 GiB.
		if maximum != e.session.memory.limit {
			// The pinned module/configuration must use the validated maximum.
			return &fixedLinearMemory{owner: &linearMemoryPool{closed: true}}
		}
		return lease
	}))
	runtimeConfig := wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(uint32(e.recipe.WASMMemoryBytes / 65536)).WithCloseOnContextDone(true).WithCompilationCache(e.cache) //nolint:gosec // Recipe validation bounds the page count to 8192.
	pool, err := webassembly.InitWithWASM(webassembly.Config{Context: ctx, WASM: wasmBytes, FSConfig: wazero.NewFSConfig(),
		RuntimeConfig: runtimeConfig, MaxTotal: 1, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		return fmt.Errorf("initialize isolated PDFium: %w", err)
	}
	defer pool.Close() //nolint:errcheck // Original processing error is authoritative; runtime still closes.
	instance, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		return fmt.Errorf("instantiate PDFium: %w", err)
	}
	defer instance.Close() //nolint:errcheck // Pool closes the runtime even after a WASM trap.
	err = fn(ctx, instance)
	if ctx.Err() != nil {
		return fmt.Errorf("PDF worker interrupted: %w", context.Cause(ctx))
	}
	return err
}

func (e *pdfiumEngine) Render(ctx context.Context, source Source, page redaction.Page, recipe redaction.Recipe) (result Raster, resultErr error) {
	if recipe != e.recipe {
		return Raster{}, errors.New("render recipe differs from qualified engine recipe")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	ctx, stop, err := watchRSS(ctx, recipe.QualifiedPeakRSSBytes)
	if err != nil {
		return Raster{}, err
	}
	defer stop()
	if _, _, err := dimensions(page, recipe); err != nil {
		return Raster{}, err
	}
	staged, err := stageSource(ctx, source, recipe.MaxStagingBytes)
	if err != nil {
		return Raster{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, staged.Close(), os.Remove(staged.Name())) }()
	err = e.withInstance(ctx, func(instance pdfium.Pdfium) error {
		doc, err := instance.OpenDocument(&requests.OpenDocument{FileReader: io.NewSectionReader(staged, 0, source.Size), FileReaderSize: source.Size})
		if err != nil {
			return fmt.Errorf("open PDF source: %w", err)
		}
		result, err = renderPage(instance, requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: page.Number - 1}}, page, recipe)
		return err
	})
	return result, err
}

func renderPage(instance pdfium.Pdfium, ref requests.Page, page redaction.Page, recipe redaction.Recipe) (Raster, error) {
	w, h, err := dimensions(page, recipe)
	if err != nil {
		return Raster{}, err
	}
	size, err := instance.GetPageSize(&requests.GetPageSize{Page: ref})
	if err != nil {
		return Raster{}, fmt.Errorf("read PDF page dimensions: %w", err)
	}
	if math.Abs(size.Width-physicalPoints(page.Width)) > 0.001 || math.Abs(size.Height-physicalPoints(page.Height)) > 0.001 {
		return Raster{}, errors.New("PDF page dimensions differ from declared frame")
	}
	rendered, err := instance.RenderPageInPixels(&requests.RenderPageInPixels{Page: ref, Width: w, Height: h, RenderForm: true})
	if err != nil {
		return Raster{}, fmt.Errorf("render PDF page: %w", err)
	}
	defer rendered.Cleanup()
	img := rendered.Result.RenderedImage
	if img.Bounds() != image.Rect(0, 0, w, h) {
		return Raster{}, errors.New("PDFium rendered dimensions differ from declared frame")
	}
	output := image.NewNRGBA(img.Bounds())
	draw.Draw(output, output.Bounds(), img, image.Point{}, draw.Src)
	return Raster{Pixels: output, Page: page, DPI: recipe.DPI}, nil
}
