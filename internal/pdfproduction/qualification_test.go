package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/redactiontest"
)

func verificationFor(pages []PageArtifact) VerificationInput {
	input := VerificationInput{}
	for _, p := range pages {
		input.Pages = append(input.Pages, p.Page)
		input.ResolvedSHA256 = append(input.ResolvedSHA256, p.ResolvedSHA256)
		input.EndorsementsSHA256 = append(input.EndorsementsSHA256, p.EndorsementsSHA256)
		input.LayoutSHA256 = append(input.LayoutSHA256, p.LayoutSHA256)
	}
	textIndex, endIndex := 0, 0
	sequence := &artifactSequence{pages: pages}
	input.NextPageArtifact = sequence.Next
	input.NextText = func(context.Context) (int, []byte, error) {
		if textIndex == len(pages) {
			return 0, nil, io.EOF
		}
		p := pages[textIndex]
		textIndex++
		var out []byte
		for _, r := range p.Runs {
			out = append(out, []byte(r.Text)...)
		}
		return p.Page.Number, out, nil
	}
	input.NextEndorsements = func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		if endIndex == len(pages) {
			return redaction.PageLayout{}, nil, io.EOF
		}
		p := pages[endIndex]
		endIndex++
		return p.Layout, p.Endorsements, nil
	}
	return input
}

func TestPDFProductionScale(t *testing.T) {
	if os.Getenv("DOCBANK_PDF_QUALIFICATION") != "1" {
		t.Skip("opt-in whole-worker 1/100/1000-page qualification")
	}
	for _, count := range []int{1, 100, 1000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPDFProductionWorker$", "-test.v") //nolint:gosec // Executes only this already-running test binary with fixed arguments.
			cmd.Env = append(os.Environ(), "DOCBANK_PDF_WORKER_PAGES="+strconv.Itoa(count))
			output, err := cmd.CombinedOutput()
			t.Log(string(output))
			require.NoError(t, err)
		})
	}
}

func TestPDFProductionMaximumPage(t *testing.T) {
	if os.Getenv("DOCBANK_PDF_QUALIFICATION") != "1" {
		t.Skip("opt-in 40-million-pixel whole-worker qualification")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPDFProductionWorker$", "-test.v") //nolint:gosec // Fixed invocation of this test executable.
	cmd.Env = append(os.Environ(), "DOCBANK_PDF_WORKER_PAGES=1", "DOCBANK_PDF_WORKER_EDGE=1")
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
}

func TestPDFProductionWorker(t *testing.T) {
	count, err := strconv.Atoi(os.Getenv("DOCBANK_PDF_WORKER_PAGES"))
	if err != nil || count < 1 {
		t.Skip("invoked by whole-worker qualification")
	}
	p, err := process.NewProcess(int32(os.Getpid()))
	require.NoError(t, err)
	var peak atomic.Uint64
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if info, err := p.MemoryInfo(); err == nil {
					for old := peak.Load(); info.RSS > old; old = peak.Load() {
						if peak.CompareAndSwap(old, info.RSS) {
							break
						}
					}
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	t.Cleanup(func() {
		if t.Failed() {
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			t.Logf("FAILED_WORKER sampled_peak_rss_bytes=%d heap_alloc_bytes=%d heap_inuse_bytes=%d heap_sys_bytes=%d", peak.Load(), memory.HeapAlloc, memory.HeapInuse, memory.HeapSys)
		}
	})
	start := time.Now()
	output, err := os.CreateTemp(t.TempDir(), "qualification-*.pdf")
	require.NoError(t, err)
	defer func() { require.NoError(t, output.Close()) }()
	sequence := &entropySequence{t: t, count: count}
	require.NoError(t, writeFresh(t.Context(), output, sequence, qualificationRecipe()))
	stat, err := output.Stat()
	require.NoError(t, err)
	width, height := entropySize()
	if count > 1 {
		require.Greater(t, stat.Size(), int64(count)*int64(width)*int64(height)*3*9/10, "high-entropy qualification must not collapse to a small deduplicated payload")
	}
	input := VerificationInput{}
	for i := 1; i <= count; i++ {
		a := entropyDescriptor(i)
		input.Pages = append(input.Pages, a.Page)
		input.ResolvedSHA256 = append(input.ResolvedSHA256, a.ResolvedSHA256)
		input.LayoutSHA256 = append(input.LayoutSHA256, a.LayoutSHA256)
		input.EndorsementsSHA256 = append(input.EndorsementsSHA256, a.EndorsementsSHA256)
	}
	textIndex, endorsementIndex := 0, 0
	input.NextText = func(context.Context) (int, []byte, error) {
		textIndex++
		if textIndex > count {
			return 0, nil, io.EOF
		}
		return textIndex, []byte(fmt.Sprintf("page-%06d", textIndex)), nil
	}
	input.NextEndorsements = func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		endorsementIndex++
		if endorsementIndex > count {
			return redaction.PageLayout{}, nil, io.EOF
		}
		a := entropyDescriptor(endorsementIndex)
		return a.Layout, a.Endorsements, nil
	}
	verificationSequence := &entropySequence{t: t, count: count}
	input.NextPageArtifact = verificationSequence.Next
	require.NoError(t, VerifyFresh(t.Context(), output, stat.Size(), input))
	cancel()
	<-done
	require.Positive(t, peak.Load(), "whole-worker RSS sampling must produce a measurement")
	finalMemory, err := p.MemoryInfo()
	require.NoError(t, err)
	kernelHWM := kernelHighWaterRSS(t)
	qualifiedPeak := max(peak.Load(), finalMemory.RSS, kernelHWM)
	require.Less(t, qualifiedPeak, uint64(1<<30))
	// The writer's private PDF and caller's output can coexist during copy.
	// Verification similarly holds caller output plus one private immutable
	// snapshot. Each sequence keeps at most one PNG file. This conservative
	// bound covers either phase without retaining all page PNGs or PDF bytes.
	stagingBound := 2*stat.Size() + sequence.maxPNGBytes + verificationSequence.maxPNGBytes
	require.LessOrEqual(t, stagingBound, qualificationRecipe().MaxStagingBytes)
	t.Logf("QUALIFICATION pages=%d pixels_per_page=%d distinct_entropy=true pdf_bytes=%d staging_upper_bound_bytes=%d max_png_bytes=%d peak_rss_bytes=%d kernel_hwm_bytes=%d elapsed=%s platform=%s/%s", count, width*height, stat.Size(), stagingBound, max(sequence.maxPNGBytes, verificationSequence.maxPNGBytes), qualifiedPeak, kernelHWM, time.Since(start), runtime.GOOS, runtime.GOARCH)
}

func kernelHighWaterRSS(tb testing.TB) uint64 {
	tb.Helper()
	if runtime.GOOS != "linux" {
		return 0
	}
	// gopsutil's MemoryInfo uses statm on Linux and leaves HWM unset. Read
	// the kernel's own process high-water value directly, not a sampled proxy.
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(tb, err)
	for line := range strings.SplitSeq(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmHWM:" && fields[2] == "kB" {
			value, err := strconv.ParseUint(fields[1], 10, 54)
			require.NoError(tb, err)
			require.Positive(tb, value)
			return value * 1024
		}
	}
	tb.Fatal("Linux kernel RSS high-water mark is unavailable")
	return 0
}

type entropySequence struct {
	t            testing.TB
	count, index int
	directory    string
	seen         map[string]bool
	maxPNGBytes  int64
}

func (s *entropySequence) Next(context.Context) (PageArtifact, error) {
	if s.index == s.count {
		return PageArtifact{}, io.EOF
	}
	s.index++
	a := entropyDescriptor(s.index)
	width, height := entropySize()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewPCG(uint64(s.index), 1)) //nolint:gosec // Reproducible synthetic pixels, not security-sensitive randomness.
	for i := 0; i < len(img.Pix); i += 4 {
		v := rng.Uint32()
		img.Pix[i] = byte(v)
		img.Pix[i+1] = byte(v >> 8)
		img.Pix[i+2] = byte(v >> 16)
		img.Pix[i+3] = 255
	}
	if s.directory == "" {
		s.directory = s.t.TempDir()
	}
	filename := filepath.Join(s.directory, "page.png")
	file, err := os.Create(filename)
	require.NoError(s.t, err)
	hasher := sha256.New()
	require.NoError(s.t, png.Encode(io.MultiWriter(file, hasher), img))
	stat, err := file.Stat()
	require.NoError(s.t, err)
	require.NoError(s.t, file.Close())
	a.PNGSize = stat.Size()
	s.maxPNGBytes = max(s.maxPNGBytes, a.PNGSize)
	a.PNGSHA256 = hex.EncodeToString(hasher.Sum(nil))
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	require.False(s.t, s.seen[a.PNGSHA256], "qualification PNGs must be distinct")
	s.seen[a.PNGSHA256] = true
	a.OpenPNG = func() (io.ReadCloser, error) { return os.Open(filename) }
	return a, nil
}

func entropyDescriptor(index int) PageArtifact {
	text := fmt.Sprintf("page-%06d", index)
	width, height := entropySize()
	// The renderer dimensions round outward. Choose the greatest physical
	// inch/10000 value whose outward 300-DPI dimension is exactly the intended
	// qualification raster, including the 40-million-pixel boundary.
	pageWidth := int64(width) * 10000 / 300
	pageHeight := int64(height) * 10000 / 300
	p := redaction.Page{Number: index, FrameSHA256: digest([]byte(fmt.Sprintf("frame %d", index))), Width: pageWidth, Height: pageHeight, Span: redaction.Span{End: int64(len(text))}}
	l := redaction.PageLayout{Source: p, Output: p}
	encoded, err := canonical.Marshal(l)
	if err != nil {
		panic(err)
	}
	return PageArtifact{Page: p, ResolvedSHA256: digest([]byte(fmt.Sprintf("plan %d", index))), Layout: l, LayoutSHA256: digest(encoded), Endorsements: []redaction.Endorsement{}, EndorsementsSHA256: digest([]byte("[]")), Runs: []redaction.Run{{Kind: "text", Text: text, Page: index, SourceSpan: &redaction.Span{End: int64(len(text))}, Boxes: []redaction.Box{{Page: index, FrameSHA256: p.FrameSHA256, X0: 1000, Y0: 1000, X1: min(pageWidth, 15000), Y1: min(pageHeight, 3000)}}, FontSizeMilliPoints: 11000}}}
}

func entropySize() (int, int) {
	if os.Getenv("DOCBANK_PDF_WORKER_EDGE") == "1" {
		return 8000, 5000
	}
	return 600, 600
}

func TestRecipeIdentityAndResourceBounds(t *testing.T) {
	for _, mutate := range []func(*redaction.Recipe){
		func(r *redaction.Recipe) { r.WriterVersion = "wrong" }, func(r *redaction.Recipe) { r.RendererSHA256 = digest([]byte("wrong")) },
		func(r *redaction.Recipe) { r.WASMMemoryBytes = 513 << 20 }, func(r *redaction.Recipe) { r.MaxStagingBytes = 101 << 30 }, func(r *redaction.Recipe) { r.PageTimeoutSeconds = 61 }, func(r *redaction.Recipe) { r.MaxPixels = 40_000_001 },
	} {
		r := qualificationRecipe()
		mutate(&r)
		_, err := NewPDFium(r)
		require.Error(t, err)
	}
}

func TestPDFiumAssetIntegrity(t *testing.T) {
	original := wasmBytes
	wasmBytes = []byte("not the qualified runtime")
	t.Cleanup(func() { wasmBytes = original })
	_, err := NewPDFium(qualificationRecipe())
	require.Error(t, err)
}

type untouchedReader struct{ reads int }

func (r *untouchedReader) ReadAt([]byte, int64) (int, error) { r.reads++; return 0, io.EOF }

func TestPDFiumRejectsTruncatedWASMFileSize(t *testing.T) {
	reader := &untouchedReader{}
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	a := unicodeArtifact(t)
	_, err = engine.Render(t.Context(), Source{Reader: reader, Size: 1 << 32, SHA256: digest([]byte("synthetic"))}, a.Page, qualificationRecipe())
	require.Error(t, err)
	require.Zero(t, reader.reads, "WASM i32 file lengths must be rejected before any reader access")
	require.Error(t, VerifyFresh(t.Context(), reader, 1<<32, verificationFor([]PageArtifact{a})))
	require.Zero(t, reader.reads)
}

type changingSource struct {
	first, second []byte
	read          bool
}

func (r *changingSource) ReadAt(p []byte, offset int64) (int, error) {
	data := r.first
	if r.read {
		data = r.second
	}
	n, err := bytes.NewReader(data).ReadAt(p, offset)
	if offset+int64(n) >= int64(len(data)) {
		r.read = true
	}
	if err != nil {
		return n, fmt.Errorf("read changing source: %w", err)
	}
	return n, nil
}

func TestPDFiumRendersOnlyVerifiedSourceBytes(t *testing.T) {
	first := redactiontest.PDF(t, []string{"PUBLIC"}, "synthetic")
	second := redactiontest.PDF(t, []string{"SECRET"}, "synthetic")
	length := max(len(first), len(second)) + 16
	first = append(first, bytes.Repeat([]byte{'\n'}, length-len(first))...)
	second = append(second, bytes.Repeat([]byte{'\n'}, length-len(second))...)
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	page := redaction.Page{Number: 1, FrameSHA256: digest([]byte("source frame")), Width: 85000, Height: 110000}
	want, err := engine.Render(t.Context(), Source{bytes.NewReader(first), int64(len(first)), digest(first)}, page, qualificationRecipe())
	require.NoError(t, err)
	got, err := engine.Render(t.Context(), Source{&changingSource{first: first, second: second}, int64(len(first)), digest(first)}, page, qualificationRecipe())
	require.NoError(t, err)
	require.True(t, bytes.Equal(want.Pixels.Pix, got.Pixels.Pix), "rendering must use the same immutable bytes that passed the source hash check")
}

func TestUnicodeFontUnavailableFailsClosed(t *testing.T) {
	a := unicodeArtifact(t)
	old := fontBytes
	fontBytes = []byte("unavailable")
	t.Cleanup(func() { fontBytes = old })
	var output bytes.Buffer
	require.Error(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	require.Empty(t, output.Bytes())
}

func TestPNGRowStreamMatchesIndependentPixels(t *testing.T) {
	a := unicodeArtifact(t)
	rows, err := openPNGRows(t.Context(), a, qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for y := range 300 {
		row, err := rows.Next()
		require.NoError(t, err)
		for x := range 300 {
			require.Equal(t, []byte{byte(x), byte(y), byte(x ^ y), 255}, row[x*4:x*4+4])
		}
	}
	require.NoError(t, rows.Finish())
}

func TestFreshRejectsAlteredEmbeddedFont(t *testing.T) {
	a := unicodeArtifact(t)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	data := output.Bytes()
	start := bytes.Index(data, fontBytes[:64])
	require.Positive(t, start)
	data[start+len(fontBytes)-1] ^= 1
	require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(data), int64(len(data)), verificationFor([]PageArtifact{a})))
}

func TestFreshComparesActualPixelsWithStagedEvidence(t *testing.T) {
	a := unicodeArtifact(t)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	reader, err := a.OpenPNG()
	require.NoError(t, err)
	decoded, err := png.Decode(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	changed := image.NewNRGBA(decoded.Bounds())
	for y := range 300 {
		for x := range 300 {
			changed.Set(x, y, decoded.At(x, y))
		}
	}
	changed.SetNRGBA(150, 150, color.NRGBA{A: 255})
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, changed))
	data := pngBytes.Bytes()
	a.PNGSHA256 = digest(data)
	a.PNGSize = int64(len(data))
	a.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{a})))
}

func freshFooterArtifact(t *testing.T) (PageArtifact, []byte) {
	t.Helper()
	a := unicodeArtifact(t)
	source := a.Page
	a.Page.Height = 15000
	a.Page.FrameSHA256 = digest([]byte("output with footer"))
	a.Layout = redaction.PageLayout{Source: source, Output: a.Page, StripHeight: 5000}
	layout, err := canonical.Marshal(a.Layout)
	require.NoError(t, err)
	a.LayoutSHA256 = digest(layout)
	img := image.NewNRGBA(image.Rect(0, 0, 300, 450))
	for y := range 450 {
		for x := range 300 {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	data := encoded.Bytes()
	a.PNGSHA256 = digest(data)
	a.PNGSize = int64(len(data))
	a.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	a.Endorsements = []redaction.Endorsement{{Kind: "number", Text: "BATES-000001", FontSHA256: fontSHA256, Color: "#000000", Box: redaction.Box{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 1000, Y0: 10500, X1: 9000, Y1: 14500}, FontSizeMilliPoints: 8000}}
	endorsements, err := canonical.Marshal(a.Endorsements)
	require.NoError(t, err)
	a.EndorsementsSHA256 = digest(endorsements)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	return a, bytes.Clone(output.Bytes())
}

func TestFreshFooterPreservesSourceOrigin(t *testing.T) {
	a, output := freshFooterArtifact(t)
	require.NoError(t, VerifyFresh(t.Context(), bytes.NewReader(output), int64(len(output)), verificationFor([]PageArtifact{a})))
	// Its source origin remains unchanged while the endorsement sits only in
	// the new 0.5-inch footer.
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	e, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	require.NoError(t, e.withInstance(t.Context(), func(instance pdfium.Pdfium) error {
		data := output
		doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
		require.NoError(t, err)
		object, err := instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}}, Index: 1})
		require.NoError(t, err)
		matrix, err := instance.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: object.PageObject})
		require.NoError(t, err)
		require.InDelta(t, 7.2, matrix.Matrix.E, 0.001)
		require.InDelta(t, 97.2, matrix.Matrix.F, 0.001)
		return nil
	}))
}

func TestVerifyFreshRejectsUnqualifiedFooterAndEndorsementKind(t *testing.T) {
	a, output := freshFooterArtifact(t)
	t.Run("footer height", func(t *testing.T) {
		candidate := a
		candidate.Layout.Source.Height += 4000
		candidate.Layout.StripHeight = 1000
		layout, err := canonical.Marshal(candidate.Layout)
		require.NoError(t, err)
		candidate.LayoutSHA256 = digest(layout)
		err = VerifyFresh(t.Context(), bytes.NewReader(output), int64(len(output)), verificationFor([]PageArtifact{candidate}))
		require.ErrorContains(t, err, "endorsement_layout_conflict")
	})
	t.Run("endorsement kind", func(t *testing.T) {
		candidate := a
		candidate.Endorsements = append([]redaction.Endorsement(nil), a.Endorsements...)
		candidate.Endorsements[0].Kind = "bates"
		endorsements, err := canonical.Marshal(candidate.Endorsements)
		require.NoError(t, err)
		candidate.EndorsementsSHA256 = digest(endorsements)
		err = VerifyFresh(t.Context(), bytes.NewReader(output), int64(len(output)), verificationFor([]PageArtifact{candidate}))
		require.ErrorContains(t, err, "endorsement_layout_conflict")
	})
}

func TestPNGRowFiltersAndIntegrity(t *testing.T) {
	for filter, data := range [][]byte{
		{0, 10, 20, 30, 40, 50, 60, 0, 15, 30, 45, 70, 90, 110},
		{1, 10, 20, 30, 30, 30, 30, 1, 15, 30, 45, 55, 60, 65},
		{2, 10, 20, 30, 40, 50, 60, 2, 5, 10, 15, 30, 40, 50},
		{3, 10, 20, 30, 35, 40, 45, 3, 10, 20, 30, 43, 50, 58},
		{4, 10, 20, 30, 30, 30, 30, 4, 5, 10, 15, 30, 40, 50},
	} {
		t.Run(strconv.Itoa(filter), func(t *testing.T) {
			a := tinyPNGArtifact(t, data)
			rows, err := openPNGRows(t.Context(), a, qualificationRecipe())
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			row, err := rows.Next()
			require.NoError(t, err)
			require.Equal(t, []byte{10, 20, 30, 255, 40, 50, 60, 255}, row)
			row, err = rows.Next()
			require.NoError(t, err)
			require.Equal(t, []byte{15, 30, 45, 255, 70, 90, 110, 255}, row)
			require.NoError(t, rows.Finish())
		})
	}
	for _, data := range [][]byte{{5, 10, 20, 30, 40, 50, 60, 0, 15, 30, 45, 70, 90, 110}, {0, 10, 20, 30, 40, 50, 60, 0, 15, 30, 45, 70, 90, 110, 1}} {
		a := tinyPNGArtifact(t, data)
		rows, err := openPNGRows(t.Context(), a, qualificationRecipe())
		require.NoError(t, err)
		_, err = rows.Next()
		if err == nil {
			_, err = rows.Next()
		}
		if err == nil {
			err = rows.Finish()
		}
		require.Error(t, err)
		require.NoError(t, rows.Close())
	}
}

func tinyPNGArtifact(t *testing.T, data []byte) PageArtifact {
	t.Helper()
	var output bytes.Buffer
	output.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, b []byte) {
		require.NoError(t, binary.Write(&output, binary.BigEndian, uint32(len(b))))
		output.WriteString(kind)
		output.Write(b)
		require.NoError(t, binary.Write(&output, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), b...))))
	}
	chunk("IHDR", []byte{0, 0, 0, 2, 0, 0, 0, 2, 8, 2, 0, 0, 0})
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, err := z.Write(data)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	chunk("IDAT", compressed.Bytes())
	chunk("IEND", nil)
	encoded := output.Bytes()
	return PageArtifact{Page: redaction.Page{Number: 1, FrameSHA256: digest([]byte("tiny")), Width: 50, Height: 50}, PNGSize: int64(len(encoded)), PNGSHA256: digest(encoded), OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }}
}

func TestPDFProductionNativePlatform(t *testing.T) {
	if expected := os.Getenv("DOCBANK_PDF_EXPECT_PLATFORM"); expected != "" {
		require.Equal(t, expected, runtime.GOOS+"/"+runtime.GOARCH)
	}
	t.Logf("native PDF production runtime: %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func TestFreshTextUsesDeclaredGeometry(t *testing.T) {
	a := unicodeArtifact(t)
	a.Endorsements = []redaction.Endorsement{{Kind: "label", Text: "PUBLIC", FontSHA256: fontSHA256, Color: "#ffffff", Box: redaction.Box{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 1000, Y0: 4000, X1: 9000, Y1: 6000}, FontSizeMilliPoints: 8000}}
	encoded, err := canonical.Marshal(a.Endorsements)
	require.NoError(t, err)
	a.EndorsementsSHA256 = digest(encoded)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	e, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	require.NoError(t, e.withInstance(t.Context(), func(instance pdfium.Pdfium) error {
		data := output.Bytes()
		doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
		require.NoError(t, err)
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}}
		object, err := instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: 1})
		require.NoError(t, err)
		matrix, err := instance.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: object.PageObject})
		require.NoError(t, err)
		size, err := instance.FPDFTextObj_GetFontSize(&requests.FPDFTextObj_GetFontSize{PageObject: object.PageObject})
		require.NoError(t, err)
		require.InDelta(t, 57.6, float64(matrix.Matrix.A)*float64(size.FontSize)*4, 0.001)
		object, err = instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: 4})
		require.NoError(t, err)
		size, err = instance.FPDFTextObj_GetFontSize(&requests.FPDFTextObj_GetFontSize{PageObject: object.PageObject})
		require.NoError(t, err)
		require.InDelta(t, 8, size.FontSize, 0.001)
		return nil
	}))
}

func TestQualifiedRSSCeiling(t *testing.T) {
	r := qualificationRecipe()
	r.QualifiedPeakRSSBytes = 1
	_, err := NewPDFium(r)
	require.Error(t, err)
	a := unicodeArtifact(t)
	var output bytes.Buffer
	require.Error(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, r))
	require.Empty(t, output.Bytes())
}

func TestFixedWASMLinearMemoryDoesNotCopyOnGrowth(t *testing.T) {
	pool := &linearMemoryPool{limit: 128}
	memory, err := pool.acquire()
	require.NoError(t, err)
	first := memory.Reallocate(64)
	first[0] = 7
	grown := memory.Reallocate(128)
	require.Same(t, &first[0], &grown[0])
	require.Equal(t, byte(7), grown[0])
	require.Equal(t, make([]byte, 64), grown[64:])
	require.Nil(t, memory.Reallocate(129))
	memory.Free()
	require.Nil(t, memory.Reallocate(1))
}

func TestWASMLinearMemoryLeaseIsolation(t *testing.T) {
	pool := &linearMemoryPool{limit: 128}
	first, err := pool.acquire()
	require.NoError(t, err)
	old := first.Reallocate(128)
	for i := range old {
		old[i] = 255
	}
	// Shrinking must not forget bytes that were accessible earlier.
	require.Len(t, first.Reallocate(16), 16)
	result := make(chan error, 1)
	go func() {
		_, err := pool.acquire()
		result <- err
	}()
	require.Error(t, <-result)
	first.Free()
	second, err := pool.acquire()
	require.NoError(t, err)
	reused := second.Reallocate(128)
	require.Same(t, &old[0], &reused[0])
	require.Equal(t, make([]byte, 128), reused)
	reused[0] = 7
	first.Free() // A stale release must not clear the current runtime.
	require.Equal(t, byte(7), reused[0])
	require.Nil(t, first.Reallocate(128))
	second.Free()
	pool.close()
	_, err = pool.acquire()
	require.Error(t, err)
	require.Nil(t, pool.buffer)
}

func TestWASMBackingSessionLifetime(t *testing.T) {
	manager := newBackingManager(128)
	first, err := manager.acquire(t.Context(), 128)
	require.NoError(t, err)
	page, err := first.memory.acquire()
	require.NoError(t, err)
	old := page.Reallocate(128)
	for i := range old {
		old[i] = 255
	}
	// Session close must also invalidate and clear a still-active page lease.
	first.release()
	second, err := manager.acquire(t.Context(), 64)
	require.NoError(t, err)
	defer second.release()
	page2, err := second.memory.acquire()
	require.NoError(t, err)
	reused := page2.Reallocate(64)
	require.Same(t, &old[0], &reused[0])
	require.Equal(t, make([]byte, 128), second.memory.buffer)
	require.Nil(t, page2.Reallocate(65), "a reused allocation must honor the smaller session limit")
	reused[0] = 7
	first.release()
	page.Free()
	require.Nil(t, page.Reallocate(1))
	require.Equal(t, byte(7), reused[0], "stale session/page owners must not clear a new session")
	_, err = first.memory.acquire()
	require.Error(t, err)
}

func TestWASMBackingConcurrentAcquireCancellation(t *testing.T) {
	manager := newBackingManager(128)
	first, err := manager.acquire(t.Context(), 128)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.acquire(ctx, 128)
		result <- err
	}()
	require.ErrorIs(t, <-result, context.DeadlineExceeded)
	first.release()
	second, err := manager.acquire(t.Context(), 128)
	require.NoError(t, err, "canceled wait must not leak the exclusive reservation")
	second.release()
	canceled, stop := context.WithCancel(t.Context())
	stop()
	_, err = manager.acquire(canceled, 128)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPDFiumCloseKeepsBackingLeaseUntilScavengeCompletes(t *testing.T) {
	manager := newBackingManager(128)
	first, err := manager.acquire(t.Context(), 128)
	require.NoError(t, err)
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	engine := &pdfiumEngine{
		gate:    make(chan struct{}, 1),
		session: first,
		scavenge: func() {
			close(cleanupStarted)
			<-allowCleanup
		},
	}
	closed := make(chan error, 1)
	go func() { closed <- engine.Close() }()
	<-cleanupStarted

	type acquireResult struct {
		lease *backingLease
		err   error
	}
	waiterStarted := make(chan struct{})
	waiter := make(chan acquireResult, 1)
	go func() {
		close(waiterStarted)
		lease, err := manager.acquire(t.Context(), 128)
		waiter <- acquireResult{lease: lease, err: err}
	}()
	<-waiterStarted
	require.Len(t, manager.token, 1, "Close must retain the global token throughout scavenging")
	select {
	case result := <-waiter:
		if result.lease != nil {
			result.lease.release()
		}
		t.Fatalf("waiting engine acquired during cleanup: %v", result.err)
	default:
	}

	close(allowCleanup)
	require.NoError(t, <-closed)
	result := <-waiter
	require.NoError(t, result.err)
	require.NotNil(t, result.lease)
	result.lease.release()
}

func TestPDFiumSequentialEnginesReuseBacking(t *testing.T) {
	first, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Close()) }()
	e1, ok := first.(*pdfiumEngine)
	require.True(t, ok)
	require.NoError(t, e1.withInstance(t.Context(), func(pdfium.Pdfium) error { return nil }))
	address := &e1.session.memory.buffer[0]
	second, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()
	e2, ok := second.(*pdfiumEngine)
	require.True(t, ok)
	require.Same(t, e1.cache, e2.cache, "concurrent engine acquisition shares only pinned compiled code")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, e2.withInstance(ctx, func(pdfium.Pdfium) error { t.Fatal("concurrent session entered WASM"); return nil }), context.DeadlineExceeded)
	require.Nil(t, e2.session, "waiting must not allocate a second backing")
	require.NoError(t, first.Close())
	require.NoError(t, e2.withInstance(t.Context(), func(pdfium.Pdfium) error { return nil }))
	require.Same(t, address, &e2.session.memory.buffer[0])
}

func TestPDFiumDeniedFilesystemAndCancellation(t *testing.T) {
	pdf := redactiontest.PDF(t, []string{"synthetic denied filesystem"}, "synthetic")
	filename := filepath.Join(t.TempDir(), "synthetic.pdf")
	require.NoError(t, os.WriteFile(filename, pdf, 0600))
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	e, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	require.NoError(t, e.withInstance(t.Context(), func(instance pdfium.Pdfium) error {
		_, err := instance.OpenDocument(&requests.OpenDocument{FilePath: &filename})
		require.Error(t, err)
		return nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = engine.Render(ctx, Source{bytes.NewReader(pdf), int64(len(pdf)), digest(pdf)}, redaction.Page{Number: 1, FrameSHA256: digest([]byte("frame")), Width: 85000, Height: 110000}, qualificationRecipe())
	require.ErrorIs(t, err, context.Canceled)
	ctx, cancel = context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	require.ErrorIs(t, e.withInstance(ctx, func(pdfium.Pdfium) error { t.Fatal("expired context entered WASM"); return nil }), context.DeadlineExceeded)
}

func TestFreshBudgetsAndPNGDecodeLimits(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	t.Setenv("TEMP", os.Getenv("TMPDIR"))
	a := unicodeArtifact(t)
	for _, test := range []struct {
		name   string
		mutate func(*PageArtifact, *redaction.Recipe)
	}{
		{"dimension overflow", func(a *PageArtifact, _ *redaction.Recipe) { a.Page.Width = math.MaxInt64 }},
		{"axis limit", func(a *PageArtifact, _ *redaction.Recipe) { a.Page.Width = 4_000_000 }},
		{"pixel limit", func(a *PageArtifact, _ *redaction.Recipe) { a.Page.Width = 2_000_000; a.Page.Height = 2_000_000 }},
		{"hash mismatch", func(a *PageArtifact, _ *redaction.Recipe) { a.PNGSHA256 = digest([]byte("wrong")) }},
		{"source box mismatch", func(a *PageArtifact, _ *redaction.Recipe) {
			a.Runs = append([]redaction.Run{}, a.Runs...)
			a.Runs[0].Boxes = []redaction.Box{{Page: 1, FrameSHA256: digest([]byte("wrong")), X1: 1000, Y1: 1000}}
		}},
		{"layout mismatch", func(a *PageArtifact, _ *redaction.Recipe) { a.Layout.Output.Height++ }},
		{"staging budget", func(_ *PageArtifact, r *redaction.Recipe) { r.MaxStagingBytes = 128 }},
		{"PNG decompression bomb", func(a *PageArtifact, _ *redaction.Recipe) {
			header := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x00\x00\x00\x00\x00\x08\x02\x00\x00\x00\x00\x00\x00\x00")
			binary.BigEndian.PutUint32(header[16:20], math.MaxUint32)
			a.PNGSize = int64(len(header))
			a.PNGSHA256 = digest(header)
			a.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(header)), nil }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, r := a, qualificationRecipe()
			test.mutate(&p, &r)
			var output bytes.Buffer
			require.Error(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{p}}, r))
			require.Empty(t, output.Bytes())
			files, err := os.ReadDir(os.Getenv("TMPDIR"))
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

type contextPageSequence struct {
	artifact PageArtifact
	done     bool
}

func (s *contextPageSequence) Next(ctx context.Context) (PageArtifact, error) {
	if s.done {
		return PageArtifact{}, io.EOF
	}
	s.done = true
	a := s.artifact
	open := a.OpenPNG
	a.OpenPNG = func() (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return open()
	}
	return a, nil
}

func TestFreshKeepsPageContextUntilArtifactConsumed(t *testing.T) {
	var output bytes.Buffer
	sequence := &contextPageSequence{artifact: unicodeArtifact(t)}
	require.NoError(t, writeFresh(t.Context(), &output, sequence, qualificationRecipe()))
}

func TestFreshDoesNotTreatTruncatedPNGAsSequenceEnd(t *testing.T) {
	first, second := entropyDescriptor(1), entropyDescriptor(2)
	img := image.NewRGBA(image.Rect(0, 0, 600, 600))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	imageBytes := encoded.Bytes()
	for _, a := range []*PageArtifact{&first, &second} {
		a.PNGSize = int64(len(imageBytes))
		a.PNGSHA256 = digest(imageBytes)
	}
	first.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(imageBytes)), nil }
	second.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil }
	var output bytes.Buffer
	require.Error(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{first, second}}, qualificationRecipe()))
	require.Empty(t, output.Bytes())
}

func TestFreshEndorsementOccurrencesAndTampering(t *testing.T) {
	a := unicodeArtifact(t)
	firstBox := redaction.Box{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 1000, Y0: 7500, X1: 9000, Y1: 9500}
	secondBox := redaction.Box{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 1000, Y0: 5000, X1: 9000, Y1: 7000}
	a.Endorsements = []redaction.Endorsement{{Kind: "label", Text: "café", FontSHA256: digest(fontBytes), Color: "#ffffff", Box: firstBox, FontSizeMilliPoints: 8000}, {Kind: "label", Text: "café", FontSHA256: digest(fontBytes), Color: "#ffffff", Box: secondBox, FontSizeMilliPoints: 8000}}
	encoded, err := canonical.Marshal(a.Endorsements)
	require.NoError(t, err)
	a.EndorsementsSHA256 = digest(encoded)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	require.NoError(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{a})))
	for _, test := range []struct{ name, old, replacement string }{
		{"wrong role", "/Endorsement BMC", "/SanitizedS BMC"},
		{"moved text", "7.2 21.6 Tm", "8.2 21.6 Tm"},
		{"visible text", "3 Tr", "0 Tr"},
		{"missing endorsement", "<0001000200030004> Tj ET", "<0001000200030004> XX ET"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Contains(t, output.String(), test.old)
			// The final occurrence belongs to the final endorsement, even when
			// source text and both endorsements contain the same string.
			offset := bytes.LastIndex(output.Bytes(), []byte(test.old))
			changed := bytes.Clone(output.Bytes())
			copy(changed[offset:], test.replacement)
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
		})
	}
}

func TestVerifyFreshIndependentEvidence(t *testing.T) {
	a := unicodeArtifact(t)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	require.NoError(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{a})))
	t.Run("expected text mismatch", func(t *testing.T) {
		input := verificationFor([]PageArtifact{a})
		input.NextText = func(context.Context) (int, []byte, error) { return 1, []byte("UNSANITIZED"), nil }
		require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), input))
	})
	t.Run("expected image mismatch", func(t *testing.T) {
		changed := a
		changed.PNGSHA256 = digest([]byte("wrong"))
		require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{changed})))
	})
	t.Run("missing evidence", func(t *testing.T) {
		input := verificationFor([]PageArtifact{a})
		input.NextEndorsements = func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
			return redaction.PageLayout{}, nil, io.EOF
		}
		require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), input))
	})
	t.Run("extra evidence", func(t *testing.T) {
		input := verificationFor([]PageArtifact{a})
		input.NextText = func(context.Context) (int, []byte, error) { return 1, []byte("café給与[REDACTED]"), nil }
		require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), input))
	})
}
