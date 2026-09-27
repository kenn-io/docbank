package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"math/big"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/image/font/sfnt"
)

//go:embed fonts/NotoSansJP.ttf
var fontBytes []byte

// BundledUnicodeFont returns a private copy of the pinned font used by the
// fresh writer so deterministic rendition layout and PDF emission share it.
func BundledUnicodeFont() []byte { return bytes.Clone(fontBytes) }

const maxTextBytes = 1 << 20

type objectWriter struct {
	out         io.Writer
	offsets     []int64
	size, limit int64
	err         error
}

// ErrOutputLimit reports refusal before fresh-PDF private staging or output
// exceeds the caller's explicit byte budget.
var ErrOutputLimit = errors.New("PDF output or private staging budget exceeded")

func (w *objectWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.limit-w.size {
		w.err = ErrOutputLimit
		return 0, w.err
	}
	n, err := w.out.Write(p)
	w.size += int64(n)
	w.err = err
	return n, err
}
func (w *objectWriter) printf(format string, args ...any) {
	if w.err == nil {
		_, w.err = fmt.Fprintf(w, format, args...)
	}
}
func (w *objectWriter) reserve() int { w.offsets = append(w.offsets, 0); return len(w.offsets) - 1 }
func (w *objectWriter) object(id int, body string) {
	w.offsets[id] = w.size
	w.printf("%d 0 obj\n%s\nendobj\n", id, body)
}
func (w *objectWriter) stream(id int, dict string, data []byte) {
	w.offsets[id] = w.size
	w.printf("%d 0 obj\n<< %s /Length %d >>\nstream\n", id, dict, len(data))
	_, _ = w.Write(data)
	w.printf("\nendstream\nendobj\n")
}

// writeFresh privately stages a fresh object graph. Nothing reaches out until
// every descriptor, PNG identity, and page sequence has been validated. At most
// one decoded page is retained, plus bounded xref/page-reference indexes.
func writeFresh(ctx context.Context, out io.Writer, pages PageSequence, recipe redaction.Recipe) (result error) {
	if err := validateRecipe(recipe); err != nil {
		return err
	}
	if out == nil || pages == nil {
		return errors.New("missing PDF output or page sequence")
	}
	if hashBytes(fontBytes) != fontSHA256 || recipe.FontSHA256 != "" && recipe.FontSHA256 != fontSHA256 {
		return errors.New("qualified Unicode font unavailable or hash mismatch")
	}
	ctx, stop, err := watchRSS(ctx, recipe.QualifiedPeakRSSBytes)
	if err != nil {
		return err
	}
	defer stop()
	font, err := sfnt.Parse(fontBytes)
	if err != nil {
		return fmt.Errorf("qualified Unicode font unavailable: %w", err)
	}
	file, err := os.CreateTemp("", "docbank-fresh-*.pdf")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close(), os.Remove(file.Name())) }()
	w := &objectWriter{out: file, limit: min(maxPayloadBytes, maxPDFFileBytes, recipe.MaxStagingBytes), offsets: []int64{0, 0, 0, 0, 0}}
	w.printf("%%PDF-1.7\n%%\xe2\xe3\xcf\xd3\n")
	w.object(1, "<< /Type /Catalog /Pages 2 0 R >>")
	w.stream(3, fmt.Sprintf("/Length1 %d", len(fontBytes)), fontBytes)
	w.object(4, "<< /Type /FontDescriptor /FontName /NotoSansJP /Flags 4 /FontBBox [-1000 -1000 3000 3000] /ItalicAngle 0 /Ascent 1000 /Descent -250 /CapHeight 700 /StemV 80 /FontFile2 3 0 R >>")
	var refs []int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageCtx, cancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
		ref, more, err := writeNextPage(pageCtx, w, pages, len(refs)+1, font, recipe)
		cancel()
		// Return discarded page rasters to the OS before the next page allocates
		// against the worker's RSS ceiling, including its reusable WASM backing.
		debug.FreeOSMemory()
		if err != nil {
			return fmt.Errorf("next PDF page: %w", err)
		}
		if !more {
			break
		}
		refs = append(refs, ref)
		if w.err != nil {
			return w.err
		}
	}
	if len(refs) == 0 {
		return errors.New("PDF must contain at least one page")
	}
	var kids strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&kids, "%d 0 R ", ref)
	}
	w.object(2, fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(refs), kids.String()))
	xref := w.reserve()
	w.offsets[xref] = w.size
	// Keep 64-bit object offsets even though this pinned WASM reader imposes
	// a stricter per-PDF size bound than the aggregate export payload ceiling.
	entries := make([]byte, len(w.offsets)*11)
	for id, offset := range w.offsets {
		if offset < 0 {
			return errors.New("invalid PDF object offset")
		}
		base := id * 11
		entries[base] = 1
		binary.BigEndian.PutUint64(entries[base+1:base+9], uint64(offset))
	}
	entries[0] = 0
	entries[9] = 255
	entries[10] = 255
	w.stream(xref, fmt.Sprintf("/Type /XRef /Size %d /Root 1 0 R /W [1 8 2]", len(w.offsets)), entries)
	w.printf("startxref\n%d\n%%%%EOF\n", w.offsets[xref])
	if w.err != nil {
		return w.err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(out, contextReader{ctx, file})
	return err
}

// A descriptor and its PNG share one page deadline. In particular, do not
// cancel the context given to Next before closing its context-bound artifact.
func writeNextPage(ctx context.Context, w *objectWriter, pages PageSequence, number int, font *sfnt.Font, recipe redaction.Recipe) (int, bool, error) {
	artifact, err := pages.Next(ctx)
	if errors.Is(err, io.EOF) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if number > maxPageCount || artifact.Page.Number != number {
		return 0, false, errors.New("PDF pages must be contiguous and within page-count limit")
	}
	if err := validateArtifact(artifact, recipe); err != nil {
		return 0, false, err
	}
	ref, err := writeArtifact(ctx, w, artifact, font, recipe)
	return ref, true, err
}

func writeArtifact(ctx context.Context, w *objectWriter, a PageArtifact, font *sfnt.Font, r redaction.Recipe) (int, error) {
	img, err := readPNG(ctx, a, r)
	if err != nil {
		return 0, err
	}
	if err := validateBoundPixels(img, a, r); err != nil {
		return 0, err
	}
	return writePage(ctx, w, img, a, font)
}

// Bound every variable-length field before canonical marshaling can allocate
// its encoded representation (JSON escaping can expand each text byte sixfold).
func preflightEndorsements(endorsements []redaction.Endorsement) error {
	if len(endorsements) > 4096 {
		return errors.New("endorsement evidence exceeds qualified bounds")
	}
	textBytes := 0
	for _, e := range endorsements {
		if len(e.Text) > maxTextBytes-textBytes || len(e.Kind) > 64 || len(e.FontSHA256) > 64 || len(e.Color) > 7 || len(e.Box.FrameSHA256) > 64 {
			return errors.New("endorsement evidence exceeds qualified bounds")
		}
		textBytes += len(e.Text)
	}
	return nil
}

func validateArtifact(a PageArtifact, r redaction.Recipe) error {
	if len(a.Runs) > 16384 || len(a.Endorsements) > 4096 {
		return errors.New("page text descriptor exceeds qualified bounds")
	}
	if err := preflightEndorsements(a.Endorsements); err != nil {
		return err
	}
	// The public verifier receives layout and endorsement authority from its
	// caller, so enforce the same closed contract used by production painting:
	// no invented strip heights or endorsement kinds may become valid merely
	// because the PDF contains matching generic text objects.
	if err := paintEndorsements(nil, a.Layout, a.Endorsements, r); err != nil {
		return err
	}
	textBound := 0
	for _, run := range a.Runs {
		if len(run.Text) > maxTextBytes-textBound || len(run.Kind) > 9 || len(run.Boxes) > 16384 || len(run.Boxes) == 0 && !whitespaceOnly(run.Text) || run.FontSizeMilliPoints <= 0 || run.FontSizeMilliPoints > 1_000_000 {
			return errors.New("sanitized evidence exceeds qualified bounds")
		}
		textBound += len(run.Text)
	}
	for _, e := range a.Endorsements {
		if len(e.Text) > maxTextBytes-textBound {
			return errors.New("page text evidence exceeds qualified bounds")
		}
		textBound += len(e.Text)
	}
	if len(a.Layout.Source.FrameSHA256) > 64 || len(a.Layout.Output.FrameSHA256) > 64 {
		return errors.New("layout evidence exceeds qualified bounds")
	}
	if _, _, err := dimensions(a.Page, r); err != nil {
		return err
	}
	if !validHash(a.PNGSHA256) || !validHash(a.ResolvedSHA256) || !validHash(a.EndorsementsSHA256) || !validHash(a.LayoutSHA256) || a.OpenPNG == nil || a.PNGSize <= 0 || a.PNGSize > r.MaxPixels*8+1<<20 {
		return errors.New("invalid page artifact identities or PNG size")
	}
	l := a.Layout
	if a.Page != l.Output || l.Source.Number != a.Page.Number || l.Source.Width != a.Page.Width || l.StripHeight < 0 || l.StripHeight > a.Page.Height || l.Source.Height != a.Page.Height-l.StripHeight || !validHash(l.Source.FrameSHA256) {
		return errors.New("invalid output page layout or source origin")
	}
	if l.Source.Span.Start < 0 || l.Source.Span.End < l.Source.Span.Start || a.Page.Span.Start < 0 || a.Page.Span.End < a.Page.Span.Start {
		return errors.New("invalid page text span")
	}
	encoded, err := canonical.Marshal(l)
	if err != nil {
		return err
	}
	if hashBytes(encoded) != a.LayoutSHA256 {
		return errors.New("page layout hash mismatch")
	}
	encoded, err = canonical.Marshal(a.Endorsements)
	if err != nil {
		return err
	}
	if hashBytes(encoded) != a.EndorsementsSHA256 {
		return errors.New("endorsement hash mismatch")
	}
	var textBytes int
	var previousEnd int64 = -1
	for _, run := range a.Runs {
		textBytes += len(run.Text)
		if textBytes > maxTextBytes || !utf8.ValidString(run.Text) || run.Text == "" || run.Page != a.Page.Number {
			return errors.New("invalid or excessive sanitized text run")
		}
		if run.Kind != "text" && run.Kind != "redaction" {
			return errors.New("unknown sanitized run kind")
		}
		if run.Kind == "text" {
			s := run.SourceSpan
			if s == nil || s.Start < l.Source.Span.Start || s.End > l.Source.Span.End || s.End <= s.Start || s.Start < previousEnd || s.End-s.Start != int64(len(run.Text)) {
				return errors.New("invalid retained source span")
			}
			previousEnd = s.End
		} else if run.SourceSpan != nil {
			return errors.New("redaction marker must not carry source text span")
		}
		for _, box := range run.Boxes {
			if !validBox(box, l.Source) {
				return errors.New("sanitized text box is outside unchanged source frame")
			}
		}
	}
	for _, e := range a.Endorsements {
		textBytes += len(e.Text)
		if textBytes > maxTextBytes || !utf8.ValidString(e.Text) || e.Text == "" || !validBox(e.Box, a.Page) || !validHash(e.FontSHA256) || e.FontSHA256 != fontSHA256 || e.FontSizeMilliPoints <= 0 || e.FontSizeMilliPoints > 1_000_000 || e.FontSizeMilliPoints > (e.Box.Y1-e.Box.Y0)*72/10 || e.Kind == "" || e.Color != "#000000" && e.Color != "#ffffff" {
			return errors.New("invalid endorsement text, font, color, or placement")
		}
	}
	return nil
}

func validBox(b redaction.Box, p redaction.Page) bool {
	return b.Page == p.Number && b.FrameSHA256 == p.FrameSHA256 && b.X0 >= 0 && b.Y0 >= 0 && b.X1 > b.X0 && b.Y1 > b.Y0 && b.X1 <= p.Width && b.Y1 <= p.Height
}

func readPNG(ctx context.Context, a PageArtifact, r redaction.Recipe) (result image.Image, err error) {
	w, h, err := dimensions(a.Page, r)
	if err != nil {
		return nil, err
	}
	reader, err := a.OpenPNG()
	if err != nil {
		return nil, fmt.Errorf("open final page PNG: %w", err)
	}
	if reader == nil {
		return nil, errors.New("nil final PNG reader")
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	hasher := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, reader}, N: a.PNGSize + 1}
	buffer, err := preflightPNG(io.TeeReader(limited, hasher), w, h)
	if err != nil {
		return nil, fmt.Errorf("read PNG header: %w", err)
	}
	result, err = png.Decode(buffer)
	if err != nil {
		return nil, fmt.Errorf("decode final page PNG: %w", err)
	}
	if _, err = io.Copy(io.Discard, buffer); err != nil {
		return nil, err
	}
	if limited.N != 1 || hex.EncodeToString(hasher.Sum(nil)) != a.PNGSHA256 {
		return nil, errors.New("final page PNG size or hash mismatch")
	}
	return result, nil
}

func writePage(ctx context.Context, w *objectWriter, img image.Image, a PageArtifact, font *sfnt.Font) (int, error) {
	page, imageID, lengthID, content, fontID, cidID, cmapID, gidID := w.reserve(), w.reserve(), w.reserve(), w.reserve(), w.reserve(), w.reserve(), w.reserve(), w.reserve()
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	w.offsets[imageID] = w.size
	w.printf("%d 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Interpolate false /Filter /FlateDecode /Length %d 0 R >>\nstream\n", imageID, width, height, lengthID)
	start := w.size
	compressed := zlib.NewWriter(w)
	row := make([]byte, width*3)
	pixels, stride, err := pixelBytes(img)
	if err != nil {
		return 0, err
	}
	for y := range height {
		if err := ctx.Err(); err != nil {
			_ = compressed.Close()
			return 0, err
		}
		for x := range width {
			pos := y*stride + x*4
			if pixels[pos+3] != 255 {
				_ = compressed.Close()
				return 0, errors.New("final burned page PNG must be opaque")
			}
			row[x*3] = pixels[pos]
			row[x*3+1] = pixels[pos+1]
			row[x*3+2] = pixels[pos+2]
		}
		if _, err := compressed.Write(row); err != nil {
			return 0, fmt.Errorf("compress page pixels: %w", err)
		}
	}
	if err := compressed.Close(); err != nil {
		return 0, fmt.Errorf("finish page pixels: %w", err)
	}
	length := w.size - start
	w.printf("\nendstream\nendobj\n")
	w.object(lengthID, strconv.FormatInt(length, 10))
	characters := []rune{0}
	codes := map[rune]uint16{}
	pageWidth, err := physicalPointDecimal(a.Page.Width)
	if err != nil {
		return 0, err
	}
	pageHeight, err := physicalPointDecimal(a.Page.Height)
	if err != nil {
		return 0, err
	}
	var source strings.Builder
	fmt.Fprintf(&source, "q %s 0 0 %s 0 0 cm /Im0 Do Q\n", pageWidth, pageHeight)
	addText := func(role, text string, b redaction.Box, size int64) error {
		var hexText strings.Builder
		for _, char := range text {
			code, ok := codes[char]
			if !ok {
				if len(characters) >= 65536 {
					return errors.New("page has too many distinct Unicode characters")
				}
				code = uint16(len(characters)) //nolint:gosec // Immediately bounded to fewer than 65536 codepoints.
				codes[char] = code
				characters = append(characters, char)
			}
			fmt.Fprintf(&hexText, "%04X", code)
		}
		// Invisible text overlays the already burned/endorsed pixels. Every run
		// has a marked role and deterministic origin; no source PDF object is reused.
		xscale := float64(b.X1-b.X0) * 72 * 100 / (float64(utf8.RuneCountInString(text)*1000) * float64(size))
		x, err := physicalPointDecimal(b.X0)
		if err != nil {
			return err
		}
		y, err := physicalPointDecimal(a.Page.Height - b.Y1)
		if err != nil {
			return err
		}
		fmt.Fprintf(&source, "/%s BMC\nBT /F0 %.3f Tf 3 Tr %.9f 0 0 1 %s %s Tm <%s> Tj ET\nEMC\n", role, float64(size)/1000, xscale, x, y, hexText.String())
		return nil
	}
	for _, run := range a.Runs {
		if err := addText("SanitizedSource", run.Text, runPlacement(run, a.Layout.Source), run.FontSizeMilliPoints); err != nil {
			return 0, err
		}
	}
	for _, e := range a.Endorsements {
		if err := addText("Endorsement", e.Text, e.Box, e.FontSizeMilliPoints); err != nil {
			return 0, err
		}
	}
	gids := make([]byte, len(characters)*2)
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /DocbankUnicode def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	var widths strings.Builder
	for i := 1; i < len(characters); {
		count := min(100, len(characters)-i)
		fmt.Fprintf(&cmap, "%d beginbfchar\n", count)
		for end := i + count; i < end; i++ {
			glyph, err := font.GlyphIndex(nil, glyphRune(characters[i]))
			if err != nil {
				return 0, fmt.Errorf("unicode font glyph lookup: %w", err)
			}
			if glyph == 0 {
				return 0, fmt.Errorf("qualified Unicode font has no glyph for U+%04X", characters[i])
			}
			binary.BigEndian.PutUint16(gids[i*2:], uint16(glyph))
			fmt.Fprintf(&cmap, "<%04X> <", i)
			for _, unit := range utf16.Encode([]rune{characters[i]}) {
				fmt.Fprintf(&cmap, "%04X", unit)
			}
			cmap.WriteString(">\n")
			fmt.Fprintf(&widths, "%d [1000] ", i)
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	w.stream(content, "/DocbankRole /PageContent", []byte(source.String()))
	w.stream(cmapID, "", []byte(cmap.String()))
	w.stream(gidID, "", gids)
	w.object(cidID, fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /NotoSansJP /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 4 0 R /CIDToGIDMap %d 0 R /DW 1000 /W [%s] >>", gidID, widths.String()))
	w.object(fontID, fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /NotoSansJP /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", cidID, cmapID))
	w.object(page, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 %d 0 R >> /Font << /F0 %d 0 R >> >> /Contents %d 0 R >>", pageWidth, pageHeight, imageID, fontID, content))
	return page, w.err
}

func physicalPoints(value int64) float64 { return float64(value) * 72 / 10000 }

func physicalPointDecimal(value int64) (string, error) {
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(72))
	negative := product.Sign() < 0
	product.Abs(product)
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(product, big.NewInt(10000), fraction)
	if !whole.IsInt64() {
		return "", errors.New("physical point coordinate exceeds decimal bounds")
	}
	result := whole.String()
	if fraction.Sign() != 0 {
		decimal := fmt.Sprintf("%04d", fraction.Int64())
		result += "." + strings.TrimRight(decimal, "0")
	}
	if negative && result != "0" {
		result = "-" + result
	}
	return result, nil
}

func pixelBytes(img image.Image) ([]byte, int, error) {
	switch p := img.(type) {
	case *image.RGBA:
		return p.Pix, p.Stride, nil
	case *image.NRGBA:
		return p.Pix, p.Stride, nil
	default:
		return nil, 0, errors.New("unexpected decoded PNG pixel format")
	}
}

// pngRows decodes the final, non-interlaced 8-bit RGB/RGBA PNG in scanline
// order. This keeps expected-pixel memory independent of page height while
// PDFium renders bounded stripes. The writer uses the independent Go decoder.
type pngRows struct {
	reader                         io.ReadCloser
	limited                        *io.LimitedReader
	hasher                         hash.Hash
	compressed                     *pngIDAT
	z                              io.ReadCloser
	current, previous, rgba        []byte
	width, height, channels, index int
	sha                            string
}

type pngIDAT struct {
	r             io.Reader
	remaining     uint32
	crc           hash.Hash32
	active, ended bool
}

func openPNGRows(ctx context.Context, a PageArtifact, recipe redaction.Recipe) (result *pngRows, resultErr error) {
	w, h, err := dimensions(a.Page, recipe)
	if err != nil {
		return nil, err
	}
	reader, err := a.OpenPNG()
	if err != nil {
		return nil, fmt.Errorf("open final PNG rows: %w", err)
	}
	if reader == nil {
		return nil, errors.New("nil final PNG reader")
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, reader.Close())
		}
	}()
	hasher := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, reader}, N: a.PNGSize + 1}
	input, err := preflightPNG(io.TeeReader(limited, hasher), w, h)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 33)
	if _, err := io.ReadFull(input, header); err != nil {
		return nil, err
	}
	channels := 3
	if header[25] == 6 {
		channels = 4
	}
	idat := &pngIDAT{r: input, crc: crc32.NewIEEE()}
	z, err := zlib.NewReader(idat)
	if err != nil {
		return nil, fmt.Errorf("open PNG row decompressor: %w", err)
	}
	return &pngRows{reader: reader, limited: limited, hasher: hasher, compressed: idat, z: z, current: make([]byte, w*channels+1), previous: make([]byte, w*channels+1), rgba: make([]byte, w*4), width: w, height: h, channels: channels, sha: a.PNGSHA256}, nil
}

func (r *pngIDAT) Read(p []byte) (int, error) {
	for r.remaining == 0 {
		if r.ended {
			return 0, io.EOF
		}
		if r.active {
			var crc [4]byte
			if _, err := io.ReadFull(r.r, crc[:]); err != nil {
				return 0, err
			}
			if binary.BigEndian.Uint32(crc[:]) != r.crc.Sum32() {
				return 0, errors.New("PNG IDAT checksum mismatch")
			}
		}
		var header [8]byte
		if _, err := io.ReadFull(r.r, header[:]); err != nil {
			return 0, err
		}
		r.remaining = binary.BigEndian.Uint32(header[:4])
		r.crc.Reset()
		_, _ = r.crc.Write(header[4:])
		r.active = true
		switch string(header[4:]) {
		case "IDAT":
			continue
		case "IEND":
			if r.remaining != 0 {
				return 0, errors.New("invalid PNG end chunk")
			}
			var crc [4]byte
			if _, err := io.ReadFull(r.r, crc[:]); err != nil {
				return 0, err
			}
			if binary.BigEndian.Uint32(crc[:]) != r.crc.Sum32() {
				return 0, errors.New("PNG end checksum mismatch")
			}
			r.ended = true
			return 0, io.EOF
		default:
			return 0, errors.New("final PNG contains unsupported chunks")
		}
	}
	count := min(uint64(len(p)), uint64(r.remaining))
	n, err := r.r.Read(p[:count])
	r.remaining -= uint32(n) //nolint:gosec // Read is limited to the current uint32-sized chunk.
	_, _ = r.crc.Write(p[:n])
	return n, err
}

func (r *pngRows) Next() ([]byte, error) {
	if r.index == r.height {
		return nil, io.EOF
	}
	if _, err := io.ReadFull(r.z, r.current); err != nil {
		return nil, fmt.Errorf("decode PNG scanline: %w", err)
	}
	filter := r.current[0]
	if filter > 4 {
		return nil, errors.New("unknown PNG scanline filter")
	}
	for i := 1; i < len(r.current); i++ {
		var left, aboveLeft byte
		above := r.previous[i]
		if i > r.channels {
			left = r.current[i-r.channels]
			aboveLeft = r.previous[i-r.channels]
		}
		switch filter {
		case 1:
			r.current[i] += left
		case 2:
			r.current[i] += above
		case 3:
			r.current[i] += byte((uint16(left) + uint16(above)) / 2) //nolint:gosec // The average of two bytes is at most 255.
		case 4:
			r.current[i] += paeth(left, above, aboveLeft)
		}
	}
	for x := range r.width {
		pos := 1 + x*r.channels
		r.rgba[x*4] = r.current[pos]
		r.rgba[x*4+1] = r.current[pos+1]
		r.rgba[x*4+2] = r.current[pos+2]
		r.rgba[x*4+3] = 255
		if r.channels == 4 {
			r.rgba[x*4+3] = r.current[pos+3]
		}
	}
	r.previous, r.current = r.current, r.previous
	r.index++
	return r.rgba, nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	da, db, dc := p-int(a), p-int(b), p-int(c)
	if da < 0 {
		da = -da
	}
	if db < 0 {
		db = -db
	}
	if dc < 0 {
		dc = -dc
	}
	if da <= db && da <= dc {
		return a
	}
	if db <= dc {
		return b
	}
	return c
}

func (r *pngRows) Finish() error {
	if r.index != r.height {
		return errors.New("PNG rows were not consumed exactly")
	}
	var extra [1]byte
	if n, err := r.z.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return errors.New("PNG has extra or invalid decompressed data")
	}
	if _, err := io.Copy(io.Discard, r.compressed); err != nil {
		return err
	}
	if !r.compressed.ended {
		return errors.New("PNG end chunk is missing")
	}
	if n, err := r.limited.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) || r.limited.N != 1 || hex.EncodeToString(r.hasher.Sum(nil)) != r.sha {
		return errors.New("final PNG size, trailing data, or hash mismatch")
	}
	return nil
}

func (r *pngRows) Close() error { return errors.Join(r.z.Close(), r.reader.Close()) }
