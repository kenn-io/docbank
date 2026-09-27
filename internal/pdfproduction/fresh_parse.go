package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"

	"go.kenn.io/docbank/document/redaction"
	"golang.org/x/image/font/sfnt"
)

const freshHeader = "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"
const freshStreamEnd = "\nendstream\nendobj\n"
const maxFreshTextObject = 8 << 20

// freshPhysicalPointDecimal independently derives the exact finite decimal
// spelling expected from the writer for inch/10000 physical coordinates.
func freshPhysicalPointDecimal(value int64) (string, error) {
	rational := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(value), big.NewInt(72)), big.NewInt(10000))
	if rational.Num().BitLen() > 63 {
		return "", errors.New("fresh physical point coordinate exceeds bounds")
	}
	result := strings.TrimRight(strings.TrimRight(rational.FloatString(4), "0"), ".")
	if result == "-0" || result == "" {
		return "0", nil
	}
	return result, nil
}

// freshIndex describes a partition of the entire file, not merely a set of
// declared xref entries. The only accepted order is this writer's order. Every
// partition is consumed completely by an exact object/stream grammar below.
type freshIndex struct {
	reader        io.ReaderAt
	offsets, ends []int64
}

func parseFreshIndex(ctx context.Context, reader io.ReaderAt, size int64, pages int) (*freshIndex, error) {
	if pages < 1 || pages > maxPageCount || size < 128 || size > maxPDFFileBytes {
		return nil, errors.New("invalid fresh PDF envelope bounds")
	}
	tail := make([]byte, 128)
	if _, err := reader.ReadAt(tail, size-128); err != nil {
		return nil, err
	}
	_, after, ok0 := bytes.CutLast(tail, []byte("startxref\n"))
	if !ok0 {
		return nil, errors.New("missing fresh PDF trailer")
	}
	startText, ok := strings.CutSuffix(string(after), "\n%%EOF\n")
	start, err := strconv.ParseInt(startText, 10, 64)
	if !ok || err != nil || startText != strconv.FormatInt(start, 10) || start < int64(len(freshHeader)) || start >= size {
		return nil, errors.New("invalid fresh PDF trailer")
	}
	count := 6 + pages*8
	head := fmt.Sprintf("%d 0 obj\n<< /Type /XRef /Size %d /Root 1 0 R /W [1 8 2] /Length %d >>\nstream\n", count-1, count, count*11)
	foot := fmt.Sprintf("%sstartxref\n%d\n%%%%EOF\n", freshStreamEnd, start)
	if size-start != int64(len(head)+count*11+len(foot)) {
		return nil, errors.New("unexplained fresh xref envelope bytes")
	}
	r := contextReader{ctx, io.NewSectionReader(reader, start, size-start)}
	if err := expectFreshBytes(r, head); err != nil {
		return nil, err
	}
	data := make([]byte, count*11)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	if err := expectFreshBytes(r, foot); err != nil {
		return nil, err
	}
	if !bytes.Equal(data[:11], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255}) {
		return nil, errors.New("invalid fresh xref free entry")
	}
	index := &freshIndex{reader: reader, offsets: make([]int64, count), ends: make([]int64, count)}
	for id := 1; id < count; id++ {
		entry := data[id*11 : (id+1)*11]
		offset := binary.BigEndian.Uint64(entry[1:9])
		if entry[0] != 1 || entry[9] != 0 || entry[10] != 0 || offset >= uint64(size) {
			return nil, errors.New("invalid fresh xref entry")
		}
		index.offsets[id] = int64(offset) //nolint:gosec // Offset is bounded by the validated positive file size.
	}
	order := make([]int, 0, count-1)
	order = append(order, 1, 3, 4)
	for page := range pages {
		base := 5 + page*8
		order = append(order, base+1, base+2, base+3, base+6, base+7, base+5, base+4, base)
	}
	order = append(order, 2, count-1)
	if index.offsets[1] != int64(len(freshHeader)) || index.offsets[count-1] != start {
		return nil, errors.New("invalid fresh object envelope")
	}
	for i, id := range order {
		end := size
		if i+1 < len(order) {
			end = index.offsets[order[i+1]]
		}
		if end <= index.offsets[id] {
			return nil, errors.New("fresh objects are reordered or overlap")
		}
		index.ends[id] = end
	}
	if err := expectFreshBytes(contextReader{ctx, io.NewSectionReader(reader, 0, int64(len(freshHeader)))}, freshHeader); err != nil {
		return nil, err
	}
	if err := index.expectObject(ctx, 1, "<< /Type /Catalog /Pages 2 0 R >>"); err != nil {
		return nil, err
	}
	if err := index.expectObject(ctx, 4, "<< /Type /FontDescriptor /FontName /NotoSansJP /Flags 4 /FontBBox [-1000 -1000 3000 3000] /ItalicAngle 0 /Ascent 1000 /Descent -250 /CapHeight 700 /StemV 80 /FontFile2 3 0 R >>"); err != nil {
		return nil, err
	}
	var kids strings.Builder
	for page := range pages {
		fmt.Fprintf(&kids, "%d 0 R ", 5+page*8)
	}
	if err := index.expectObject(ctx, 2, fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", pages, kids.String())); err != nil {
		return nil, err
	}
	fontHead := fmt.Sprintf("3 0 obj\n<< /Length1 %d /Length %d >>\nstream\n", len(fontBytes), len(fontBytes))
	if index.ends[3]-index.offsets[3] != int64(len(fontHead)+len(fontBytes)+len(freshStreamEnd)) {
		return nil, errors.New("invalid embedded font envelope")
	}
	fontReader := contextReader{ctx, index.section(3)}
	if err := expectFreshBytes(fontReader, fontHead); err != nil {
		return nil, err
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, fontReader, int64(len(fontBytes))); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != fontSHA256 {
		return nil, errors.New("fresh embedded font identity mismatch")
	}
	if err := expectFreshBytes(fontReader, freshStreamEnd); err != nil {
		return nil, err
	}
	return index, nil
}

func expectFreshBytes(r io.Reader, expected string) error {
	buffer := make([]byte, min(32768, len(expected)))
	for len(expected) != 0 {
		n := min(len(buffer), len(expected))
		if _, err := io.ReadFull(r, buffer[:n]); err != nil {
			return err
		}
		if string(buffer[:n]) != expected[:n] {
			return errors.New("unexpected fresh PDF grammar or unexplained bytes")
		}
		expected = expected[n:]
	}
	return nil
}

func (f *freshIndex) section(id int) *io.SectionReader {
	return io.NewSectionReader(f.reader, f.offsets[id], f.ends[id]-f.offsets[id])
}

func (f *freshIndex) expectObject(ctx context.Context, id int, body string) error {
	expected := fmt.Sprintf("%d 0 obj\n%s\nendobj\n", id, body)
	if f.ends[id]-f.offsets[id] != int64(len(expected)) {
		return fmt.Errorf("unexpected fresh object %d extent", id)
	}
	return expectFreshBytes(contextReader{ctx, f.section(id)}, expected)
}

func (f *freshIndex) smallObject(ctx context.Context, id int) ([]byte, error) {
	n := f.ends[id] - f.offsets[id]
	if n <= 0 || n > maxFreshTextObject {
		return nil, errors.New("fresh text object exceeds bounds")
	}
	data := make([]byte, n)
	_, err := io.ReadFull(contextReader{ctx, f.section(id)}, data)
	return data, err
}

func (f *freshIndex) stream(ctx context.Context, id int, dict string) ([]byte, error) {
	data, err := f.smallObject(ctx, id)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("%d 0 obj\n<< %s /Length ", id, dict)
	rest, ok := bytes.CutPrefix(data, []byte(prefix))
	if !ok {
		return nil, errors.New("unexpected fresh stream dictionary")
	}
	length, rest, ok := bytes.Cut(rest, []byte(" >>\nstream\n"))
	n, err := strconv.Atoi(string(length))
	if !ok || err != nil || n < 0 || string(length) != strconv.Itoa(n) || n > len(rest) || len(rest)-n != len(freshStreamEnd) || string(rest[n:]) != freshStreamEnd {
		return nil, errors.New("invalid fresh stream extent or trailing bytes")
	}
	return rest[:n], nil
}

type freshPage struct {
	advances []float64 // Sum of the actual parsed CID widths for each occurrence.
	image    *io.SectionReader
}

// The image stream must also have exactly the writer's encoding. Merely
// inflating it or rendering it would accept unused compressed/trailing bytes.
// Re-encode independent expected PNG rows into a bounded byte comparator; no
// second page-sized image or compressed payload is retained.
type freshImageComparison struct {
	actual      io.Reader
	buffer, rgb []byte
	z           *zlib.Writer
}

func newFreshImageComparison(ctx context.Context, actual io.Reader, width int) *freshImageComparison {
	c := &freshImageComparison{actual: contextReader{ctx, actual}, buffer: make([]byte, 32768), rgb: make([]byte, width*3)}
	c.z = zlib.NewWriter(c)
	return c
}

func (c *freshImageComparison) Write(p []byte) (int, error) {
	total := 0
	for len(p) != 0 {
		n := min(len(p), len(c.buffer))
		if _, err := io.ReadFull(c.actual, c.buffer[:n]); err != nil {
			return total, err
		}
		if !bytes.Equal(c.buffer[:n], p[:n]) {
			return total, errors.New("fresh image stream differs from canonical staged pixels")
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (c *freshImageComparison) row(rgba []byte) error {
	for x := range len(c.rgb) / 3 {
		if rgba[x*4+3] != 255 {
			return errors.New("final page pixels must be opaque")
		}
		copy(c.rgb[x*3:x*3+3], rgba[x*4:x*4+3])
	}
	_, err := c.z.Write(c.rgb)
	if err != nil {
		return fmt.Errorf("compare canonical image row: %w", err)
	}
	return nil
}

func (c *freshImageComparison) finish() error {
	if err := c.z.Close(); err != nil {
		return fmt.Errorf("finish canonical image comparison: %w", err)
	}
	var extra [1]byte
	if n, err := c.actual.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return errors.New("fresh image has unexplained compressed bytes")
	}
	return nil
}

func (f *freshIndex) page(ctx context.Context, number int, a PageArtifact, recipe redaction.Recipe) (*freshPage, error) {
	base := 5 + number*8
	w, h, err := dimensions(a.Page, recipe)
	if err != nil {
		return nil, err
	}
	pageWidth, err := freshPhysicalPointDecimal(a.Page.Width)
	if err != nil {
		return nil, err
	}
	pageHeight, err := freshPhysicalPointDecimal(a.Page.Height)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 %d 0 R >> /Font << /F0 %d 0 R >> >> /Contents %d 0 R >>", pageWidth, pageHeight, base+1, base+4, base+3)
	if err := f.expectObject(ctx, base, body); err != nil {
		return nil, err
	}
	if err := f.expectObject(ctx, base+4, fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /NotoSansJP /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", base+5, base+6)); err != nil {
		return nil, err
	}
	lengthObject, err := f.smallObject(ctx, base+2)
	if err != nil {
		return nil, err
	}
	lengthText, ok := strings.CutPrefix(string(lengthObject), fmt.Sprintf("%d 0 obj\n", base+2))
	lengthText, ended := strings.CutSuffix(lengthText, "\nendobj\n")
	length, err := strconv.ParseInt(lengthText, 10, 64)
	if !ok || !ended || err != nil || length <= 0 || length > int64(w)*int64(h)*4+1<<20 || lengthText != strconv.FormatInt(length, 10) {
		return nil, errors.New("invalid fresh image length object")
	}
	head := fmt.Sprintf("%d 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Interpolate false /Filter /FlateDecode /Length %d 0 R >>\nstream\n", base+1, w, h, base+2)
	if f.ends[base+1]-f.offsets[base+1] != int64(len(head))+length+int64(len(freshStreamEnd)) {
		return nil, errors.New("unexpected fresh image stream extent")
	}
	if err := expectFreshBytes(contextReader{ctx, f.section(base + 1)}, head); err != nil {
		return nil, err
	}
	imageStart := f.offsets[base+1] + int64(len(head))
	if err := expectFreshBytes(contextReader{ctx, io.NewSectionReader(f.reader, imageStart+length, int64(len(freshStreamEnd)))}, freshStreamEnd); err != nil {
		return nil, err
	}
	cmap, err := f.stream(ctx, base+6, "")
	if err != nil {
		return nil, err
	}
	unicode, err := parseFreshCMap(ctx, cmap)
	if err != nil {
		return nil, err
	}
	gids, err := f.stream(ctx, base+7, "")
	if err != nil {
		return nil, err
	}
	widths, err := f.fontWidths(ctx, base+5, base+7, unicode, gids)
	if err != nil {
		return nil, err
	}
	content, err := f.stream(ctx, base+3, "/DocbankRole /PageContent")
	if err != nil {
		return nil, err
	}
	advances, err := parseFreshContent(ctx, content, unicode, widths, a)
	if err != nil {
		return nil, err
	}
	return &freshPage{advances: advances, image: io.NewSectionReader(f.reader, imageStart, length)}, nil
}

func (f *freshIndex) fontWidths(ctx context.Context, id, gidID int, unicode map[uint16]string, gids []byte) (map[uint16]int, error) {
	if len(gids) != (len(unicode)+1)*2 || gids[0] != 0 || gids[1] != 0 {
		return nil, errors.New("invalid fresh CIDToGIDMap extent")
	}
	font, err := sfnt.Parse(fontBytes)
	if err != nil {
		return nil, fmt.Errorf("parse pinned CID font: %w", err)
	}
	for cid, text := range unicode {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		glyph, err := font.GlyphIndex(nil, glyphRune([]rune(text)[0]))
		if err != nil || glyph == 0 || binary.BigEndian.Uint16(gids[int(cid)*2:]) != uint16(glyph) {
			return nil, errors.New("fresh CID glyph differs from pinned Unicode font")
		}
	}
	data, err := f.smallObject(ctx, id)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("%d 0 obj\n<< /Type /Font /Subtype /CIDFontType2 /BaseFont /NotoSansJP /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 4 0 R /CIDToGIDMap %d 0 R /DW 1000 /W [", id, gidID)
	rest, ok := strings.CutPrefix(string(data), prefix)
	if !ok {
		return nil, errors.New("unexpected fresh CID font dictionary")
	}
	widths := make(map[uint16]int, len(unicode))
	for cid := 1; cid <= len(unicode); cid++ {
		rest, ok = strings.CutPrefix(rest, strconv.Itoa(cid)+" [")
		widthText, tail, closed := strings.Cut(rest, "] ")
		width, err := strconv.Atoi(widthText)
		if !ok || !closed || err != nil || widthText != strconv.Itoa(width) || width != 1000 {
			return nil, errors.New("unexpected fresh CID width")
		}
		widths[uint16(cid)] = width
		rest = tail
	}
	if rest != "] >>\nendobj\n" {
		return nil, errors.New("extra fresh CID widths or dictionary bytes")
	}
	return widths, nil
}

func parseFreshContent(ctx context.Context, content []byte, unicode map[uint16]string, widths map[uint16]int, a PageArtifact) ([]float64, error) {
	pageWidth, err := freshPhysicalPointDecimal(a.Page.Width)
	if err != nil {
		return nil, err
	}
	pageHeight, err := freshPhysicalPointDecimal(a.Page.Height)
	if err != nil {
		return nil, err
	}
	first := fmt.Sprintf("q %s 0 0 %s 0 0 cm /Im0 Do Q\n", pageWidth, pageHeight)
	rest, ok := strings.CutPrefix(string(content), first)
	if !ok {
		return nil, errors.New("unexpected fresh image content operators")
	}
	used := make(map[uint16]bool)
	advances := make([]float64, 0, len(a.Runs)+len(a.Endorsements))
	for i := range len(a.Runs) + len(a.Endorsements) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		role := "SanitizedSource"
		var text string
		var box redaction.Box
		var size int64
		if i < len(a.Runs) {
			run := a.Runs[i]
			text, box = run.Text, runPlacement(run, a.Layout.Source)
			size = run.FontSizeMilliPoints
		} else {
			e := a.Endorsements[i-len(a.Runs)]
			role, text, box, size = "Endorsement", e.Text, e.Box, e.FontSizeMilliPoints
		}
		rest, ok = strings.CutPrefix(rest, "/"+role+" BMC\n")
		if !ok {
			return nil, errors.New("unexpected fresh content role")
		}
		line, tail, ok := strings.Cut(rest, "\nEMC\n")
		if !ok {
			return nil, errors.New("unterminated fresh text occurrence")
		}
		_, after, ok0 := strings.Cut(line, " Tm <")
		if !ok0 {
			return nil, errors.New("missing fresh text codes")
		}
		encoded, ok := strings.CutSuffix(after, "> Tj ET")
		if !ok || len(encoded) == 0 || len(encoded)%4 != 0 {
			return nil, errors.New("invalid fresh text codes")
		}
		var decoded strings.Builder
		advance := 0
		for pos := 0; pos < len(encoded); pos += 4 {
			if pos%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			cid, err := strconv.ParseUint(encoded[pos:pos+4], 16, 16)
			value, exists := unicode[uint16(cid)]
			if err != nil || !exists || fmt.Sprintf("%04X", cid) != encoded[pos:pos+4] {
				return nil, errors.New("invalid fresh text CID")
			}
			if !used[uint16(cid)] && int(cid) != len(used)+1 {
				return nil, errors.New("fresh CID first-use order mismatch")
			}
			used[uint16(cid)] = true
			decoded.WriteString(value)
			advance += widths[uint16(cid)]
		}
		if decoded.String() != text || advance <= 0 {
			return nil, errors.New("fresh decoded text differs from sanitized evidence")
		}
		xscale := float64(box.X1-box.X0) * 72 * 100 / (float64(advance) * float64(size))
		x, err := freshPhysicalPointDecimal(box.X0)
		if err != nil {
			return nil, err
		}
		y, err := freshPhysicalPointDecimal(a.Page.Height - box.Y1)
		if err != nil {
			return nil, err
		}
		expected := fmt.Sprintf("BT /F0 %.3f Tf 3 Tr %.9f 0 0 1 %s %s Tm <%s> Tj ET", float64(size)/1000, xscale, x, y, encoded)
		if line != expected {
			return nil, errors.New("unexpected fresh text operators or geometry")
		}
		advances = append(advances, float64(advance))
		rest = tail
	}
	if rest != "" || len(used) != len(unicode) {
		return nil, errors.New("unexplained fresh text content or unused mappings")
	}
	return advances, nil
}

const cmapPrefix = "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /DocbankUnicode def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"
const cmapSuffix = "endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n"

// parseFreshCMap accepts only the complete program emitted by the fresh writer.
// A destination is exactly one Unicode scalar, never a partial surrogate or a
// sequence whose replacement-character decoding might conceal malformed input.
func parseFreshCMap(ctx context.Context, data []byte) (map[uint16]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > 7<<20 {
		return nil, errors.New("oversized ToUnicode program")
	}
	rest, ok := strings.CutPrefix(string(data), cmapPrefix)
	if !ok {
		return nil, errors.New("invalid ToUnicode preamble")
	}
	result := make(map[uint16]string)
	scalars := make(map[rune]bool)
	previousCount := 100
	for rest != cmapSuffix {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, tail, ok := strings.Cut(rest, "\n")
		countText, valid := strings.CutSuffix(line, " beginbfchar")
		count, err := strconv.Atoi(countText)
		if !ok || !valid || err != nil || count < 1 || count > 100 || previousCount != 100 || countText != strconv.Itoa(count) || len(result)+count > 65535 {
			return nil, errors.New("invalid ToUnicode block")
		}
		rest = tail
		previousCount = count
		for range count {
			line, rest, ok = strings.Cut(rest, "\n")
			prefix := fmt.Sprintf("<%04X> <", len(result)+1)
			destination, valid := strings.CutPrefix(line, prefix)
			destination, closed := strings.CutSuffix(destination, ">")
			if !ok || !valid || !closed || len(destination) != 4 && len(destination) != 8 {
				return nil, errors.New("invalid ToUnicode mapping length or CID")
			}
			for _, c := range destination {
				if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
					return nil, errors.New("invalid ToUnicode hexadecimal syntax")
				}
			}
			first, _ := strconv.ParseUint(destination[:4], 16, 16) // Validated hexadecimal above.
			value := rune(first)
			if len(destination) == 8 {
				second, _ := strconv.ParseUint(destination[4:], 16, 16)
				if first < 0xD800 || first > 0xDBFF || second < 0xDC00 || second > 0xDFFF {
					return nil, errors.New("invalid ToUnicode surrogate pair")
				}
				value = utf16.DecodeRune(rune(first), rune(second))
			} else if utf16.IsSurrogate(value) {
				return nil, errors.New("isolated ToUnicode surrogate")
			}
			if scalars[value] {
				return nil, errors.New("duplicate ToUnicode destination")
			}
			scalars[value] = true
			result[uint16(len(result)+1)] = string(value) //nolint:gosec // Block preflight bounds the total to 65535.
		}
		rest, ok = strings.CutPrefix(rest, "endbfchar\n")
		if !ok {
			return nil, errors.New("unterminated ToUnicode block")
		}
	}
	return result, nil
}
