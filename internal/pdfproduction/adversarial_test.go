package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// rewriteFreshFixture is a test-only byte editor, independent of the production
// parser. It preserves the writer fixture's physical order while rebuilding
// offsets, so an attack cannot pass a test merely because its xref is broken.
func rewriteFreshFixture(tb testing.TB, original []byte, pages int, edit func(map[int][]byte), unindexed []byte) []byte {
	tb.Helper()
	tail := bytes.LastIndex(original, []byte("startxref\n"))
	require.Positive(tb, tail)
	end := bytes.IndexByte(original[tail+10:], '\n')
	start, err := strconv.Atoi(string(original[tail+10 : tail+10+end]))
	require.NoError(tb, err)
	dataStart := start + bytes.Index(original[start:], []byte("\nstream\n")) + 8
	count := 6 + 8*pages
	type location struct{ id, offset int }
	locations := make([]location, 0, count-2)
	for id := 1; id < count-1; id++ {
		offset := binary.BigEndian.Uint64(original[dataStart+id*11+1 : dataStart+id*11+9])
		require.Less(tb, offset, uint64(len(original)))
		locations = append(locations, location{id, int(offset)})
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].offset < locations[j].offset })
	objects := map[int][]byte{0: bytes.Clone(original[:locations[0].offset])}
	for i, loc := range locations {
		limit := start
		if i+1 < len(locations) {
			limit = locations[i+1].offset
		}
		objects[loc.id] = bytes.Clone(original[loc.offset:limit])
	}
	edit(objects)
	var out bytes.Buffer
	out.Write(objects[0])
	xref := make([]byte, count*11)
	xref[9], xref[10] = 255, 255
	for _, loc := range locations {
		xref[loc.id*11] = 1
		binary.BigEndian.PutUint64(xref[loc.id*11+1:], uint64(out.Len()))
		out.Write(objects[loc.id])
	}
	out.Write(unindexed)
	start = out.Len()
	xref[(count-1)*11] = 1
	binary.BigEndian.PutUint64(xref[(count-1)*11+1:], uint64(start))
	fmt.Fprintf(&out, "%d 0 obj\n<< /Type /XRef /Size %d /Root 1 0 R /W [1 8 2] /Length %d >>\nstream\n", count-1, count, len(xref))
	out.Write(xref)
	fmt.Fprintf(&out, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", start)
	return out.Bytes()
}

func rewriteFixtureStream(tb testing.TB, object []byte, edit func([]byte) []byte) []byte {
	tb.Helper()
	start := bytes.Index(object, []byte("\nstream\n"))
	end := bytes.LastIndex(object, []byte("\nendstream\nendobj\n"))
	require.Positive(tb, start)
	require.Greater(tb, end, start)
	data := edit(bytes.Clone(object[start+8 : end]))
	length := bytes.LastIndex(object[:start], []byte("/Length "))
	require.Positive(tb, length)
	header := string(object[:length]) + fmt.Sprintf("/Length %d >>\nstream\n", len(data))
	return append(append([]byte(header), data...), object[end:]...)
}

func freshFixture(tb testing.TB) ([]byte, PageArtifact) {
	tb.Helper()
	a := unicodeArtifact(tb)
	var out bytes.Buffer
	require.NoError(tb, writeFresh(tb.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, qualificationRecipe()))
	return out.Bytes(), a
}

func TestVerifyMalformedToUnicodeNeverPanics(t *testing.T) {
	original, a := freshFixture(t)
	for _, destination := range []string{"00630", "006300", "0063000", "D800", "DC00", "D8000063", "0063DC00"} {
		t.Run(destination, func(t *testing.T) {
			changed := rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
				objects[11] = rewriteFixtureStream(t, objects[11], func(data []byte) []byte {
					require.Contains(t, string(data), "<0001> <0063>")
					return bytes.Replace(data, []byte("<0001> <0063>"), []byte("<0001> <"+destination+">"), 1)
				})
			}, nil)
			var err error
			require.NotPanics(t, func() {
				err = VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a}))
			})
			require.Error(t, err)
		})
	}
}

func TestVerifyRejectsMalformedToUnicodeSyntax(t *testing.T) {
	original, a := freshFixture(t)
	for _, test := range []struct{ name, from, to string }{
		{"block operator", "beginbfchar", "notbfchar"},
		{"truncated program", "endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n", ""},
		{"extra mapping", "endbfchar\n", "<FFFF> <0053>\nendbfchar\n"},
		{"trailing source", "end\nend\n", "end\nend\nSYNTHETIC PRIVATE SOURCE\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
				objects[11] = rewriteFixtureStream(t, objects[11], func(data []byte) []byte {
					require.Contains(t, string(data), test.from)
					return []byte(strings.Replace(string(data), test.from, test.to, 1))
				})
			}, nil)
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
		})
	}
}

func FuzzFreshCMap(f *testing.F) {
	for _, data := range []string{cmapPrefix + cmapSuffix, cmapPrefix + "1 beginbfchar\n<0001> <00630>\nendbfchar\n" + cmapSuffix, cmapPrefix + "1 beginbfchar\n<0001> <D83DDE00>\nendbfchar\n" + cmapSuffix, "", "<FFFF> <D800>"} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseFreshCMap(t.Context(), data)
	})
}

func TestVerifyRejectsUnexplainedPDFBytes(t *testing.T) {
	original, a := freshFixture(t)
	for _, test := range []struct {
		name     string
		id       int
		from, to string
	}{
		{"catalog private data", 1, "/Pages 2 0 R", "/Pages 2 0 R /DocbankPrivate (SYNTHETIC PRIVATE SOURCE)"},
		{"catalog action", 1, "/Pages 2 0 R", "/Pages 2 0 R /OpenAction << /S /JavaScript /JS (app.alert(1)) >>"},
		{"catalog metadata", 1, "/Pages 2 0 R", "/Pages 2 0 R /Metadata 11 0 R"},
		{"catalog attachment", 1, "/Pages 2 0 R", "/Pages 2 0 R /Names << /EmbeddedFiles 11 0 R >>"},
		{"duplicate catalog key", 1, "/Pages 2 0 R", "/Pages 2 0 R /Pages 2 0 R"},
		{"alternate page content", 5, "/Contents 8 0 R", "/Contents 8 0 R /AA << /O 11 0 R >>"},
		{"page private data", 5, "/Type /Page", "/DocbankPrivate (SYNTHETIC PRIVATE SOURCE) /Type /Page"},
		{"page tree private data", 2, "/Count 1", "/Count 1 /DocbankPrivate (SYNTHETIC PRIVATE SOURCE)"},
		{"font descriptor private data", 4, "/Flags 4", "/Flags 4 /DocbankPrivate (SYNTHETIC PRIVATE SOURCE)"},
		{"image alternate", 6, "/Interpolate false", "/Interpolate false /Alternates [<< /Image 6 0 R >>]"},
		{"font alternate", 9, "/Encoding /Identity-H", "/Encoding /Identity-H /DocbankPrivate (SYNTHETIC PRIVATE SOURCE)"},
		{"font stream metadata", 3, "/Length1", "/Metadata 11 0 R /Length1"},
		{"content dictionary private data", 8, "/DocbankRole /PageContent", "/DocbankRole /PageContent /DocbankPrivate (SYNTHETIC PRIVATE SOURCE)"},
		{"cmap dictionary private data", 11, "<<  /Length", "<< /DocbankPrivate (SYNTHETIC PRIVATE SOURCE) /Length"},
		{"prefix old revision", 0, "%PDF-1.7", "%PDF-1.4\nSYNTHETIC PRIVATE SOURCE\n%%EOF\n%PDF-1.7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
				require.Contains(t, string(objects[test.id]), test.from)
				objects[test.id] = bytes.Replace(objects[test.id], []byte(test.from), []byte(test.to), 1)
			}, nil)
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
		})
	}
	for _, extra := range []string{"999999 0 obj\n(SYNTHETIC PRIVATE SOURCE)\nendobj\n", "% SYNTHETIC PRIVATE SOURCE\n", "\n"} {
		t.Run("unindexed "+extra[:1], func(t *testing.T) {
			changed := rewriteFreshFixture(t, original, 1, func(map[int][]byte) {}, []byte(extra))
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
		})
	}
}

func TestVerifyRejectsAlteredCIDMetricsAndMapping(t *testing.T) {
	original, a := freshFixture(t)
	for _, test := range []struct {
		name string
		edit func(map[int][]byte)
	}{
		{"width", func(objects map[int][]byte) {
			objects[10] = bytes.Replace(objects[10], []byte("/W [1 [1000]"), []byte("/W [1 [2000]"), 1)
		}},
		{"default width", func(objects map[int][]byte) {
			objects[10] = bytes.Replace(objects[10], []byte("/DW 1000"), []byte("/DW 2000"), 1)
		}},
		{"glyph mapping", func(objects map[int][]byte) {
			objects[12] = rewriteFixtureStream(t, objects[12], func(data []byte) []byte { data[3] ^= 1; return data })
		}},
		{"extra glyph", func(objects map[int][]byte) {
			objects[12] = rewriteFixtureStream(t, objects[12], func(data []byte) []byte { return append(data, 0, 1) })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteFreshFixture(t, original, 1, test.edit, nil)
			require.NotEqual(t, original, changed)
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
		})
	}
}

func TestVerifyRejectsHiddenImageStreamBytes(t *testing.T) {
	original, a := freshFixture(t)
	changed := rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
		start := bytes.Index(objects[6], []byte("\nstream\n")) + 8
		end := bytes.LastIndex(objects[6], []byte(freshStreamEnd))
		extra := []byte("SYNTHETIC PRIVATE SOURCE")
		objects[6] = append(append(bytes.Clone(objects[6][:end]), extra...), objects[6][end:]...)
		objects[7] = fmt.Appendf(nil, "7 0 obj\n%d\nendobj\n", end-start+len(extra))
	}, nil)
	require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor([]PageArtifact{a})))
}

func pngChunk(kind string, data []byte) []byte {
	chunk := make([]byte, len(data)+12)
	binary.BigEndian.PutUint32(chunk, uint32(len(data)))
	copy(chunk[4:8], kind)
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return chunk
}

func TestWriterAndVerifierRejectUnsupportedPNGGrammar(t *testing.T) {
	original, a := freshFixture(t)
	r, err := a.OpenPNG()
	require.NoError(t, err)
	pngData, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"ancillary before IDAT", func(data []byte) []byte {
			return append(append(bytes.Clone(data[:33]), pngChunk("tEXt", []byte("Synthetic\x00Private source"))...), data[33:]...)
		}},
		{"ancillary after IDAT", func(data []byte) []byte {
			end := len(data) - 12
			return append(append(bytes.Clone(data[:end]), pngChunk("tEXt", []byte("Synthetic\x00Private source"))...), data[end:]...)
		}},
		{"interlaced", func(data []byte) []byte { return validInterlacedPNG(t, data) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := test.change(bytes.Clone(pngData))
			artifact := a
			artifact.PNGSize, artifact.PNGSHA256 = int64(len(changed)), digest(changed)
			artifact.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(changed)), nil }
			var output bytes.Buffer
			err := writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{artifact}}, qualificationRecipe())
			require.Error(t, err)
			require.Empty(t, output.Bytes(), "unsupported PNG must fail before published output")
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(original), int64(len(original)), verificationFor([]PageArtifact{artifact})))
		})
	}
}

// Build a valid Adam7 image, not merely a noninterlaced payload with its header
// bit flipped. The standard decoder independently confirms the fixture itself.
func validInterlacedPNG(t *testing.T, original []byte) []byte {
	t.Helper()
	header := bytes.Clone(original[:33])
	require.Equal(t, byte(2), header[25])
	header[28] = 1
	binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	for _, pass := range [][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}} {
		for y := pass[1]; y < 300; y += pass[3] {
			row := []byte{0}
			for x := pass[0]; x < 300; x += pass[2] {
				row = append(row, byte(x), byte(y), byte(x^y))
			}
			_, err := z.Write(row)
			require.NoError(t, err)
		}
	}
	require.NoError(t, z.Close())
	data := append(append(header, pngChunk("IDAT", compressed.Bytes())...), pngChunk("IEND", nil)...)
	_, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err, "interlaced fixture must be a valid PNG")
	return data
}

func renumberArtifact(t *testing.T, a PageArtifact, number int) PageArtifact {
	t.Helper()
	a.Page.Number = number
	a.Layout.Source.Number, a.Layout.Output.Number = number, number
	a.Runs = append([]redaction.Run(nil), a.Runs...)
	for i := range a.Runs {
		a.Runs[i].Page = number
		a.Runs[i].Boxes = append([]redaction.Box(nil), a.Runs[i].Boxes...)
		for j := range a.Runs[i].Boxes {
			a.Runs[i].Boxes[j].Page = number
		}
	}
	layout, err := canonical.Marshal(a.Layout)
	require.NoError(t, err)
	a.LayoutSHA256 = digest(layout)
	return a
}

func TestVerifyFreezesFinalBytesBetweenChecksAndPages(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			a := unicodeArtifact(t)
			pages := []PageArtifact{a}
			if count == 2 {
				pages = append(pages, renumberArtifact(t, a, 2))
			}
			var out bytes.Buffer
			require.NoError(t, writeFresh(t.Context(), &out, &artifactSequence{pages: pages}, qualificationRecipe()))
			original := out.Bytes()
			changed := rewriteFreshFixture(t, original, count, func(objects map[int][]byte) {
				id := 8 + (count-1)*8
				objects[id] = bytes.Replace(objects[id], []byte("Tm <0001"), []byte("Tm <0002"), 1)
			}, nil)
			require.Len(t, changed, len(original))
			require.NotEqual(t, original, changed)
			input := verificationFor(pages)
			next := input.NextPageArtifact
			calls := 0
			input.NextPageArtifact = func(ctx context.Context) (PageArtifact, error) {
				calls++
				if calls == count {
					copy(original, changed)
				}
				return next(ctx)
			}
			require.NoError(t, VerifyFresh(t.Context(), bytes.NewReader(original), int64(len(original)), input), "all passes must use the original private snapshot, never the callback-mutated reader")
			require.Equal(t, changed, original)
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(original), int64(len(original)), verificationFor(pages)), "a new verification must reject the now-mutated artifact")
		})
	}
}

func TestVerifyEvidenceCarriesOnePageDeadline(t *testing.T) {
	original, a := freshFixture(t)
	input := verificationFor([]PageArtifact{a})
	var deadline time.Time
	check := func(ctx context.Context) {
		got, ok := ctx.Deadline()
		require.True(t, ok, "every evidence callback must receive the page deadline")
		if deadline.IsZero() {
			deadline = got
		} else {
			require.Equal(t, deadline, got, "do not renew the deadline between evidence callbacks")
		}
	}
	nextArtifact, nextText, nextEndorsements := input.NextPageArtifact, input.NextText, input.NextEndorsements
	input.NextPageArtifact = func(ctx context.Context) (PageArtifact, error) { check(ctx); return nextArtifact(ctx) }
	input.NextText = func(ctx context.Context) (int, []byte, error) { check(ctx); return nextText(ctx) }
	input.NextEndorsements = func(ctx context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		check(ctx)
		return nextEndorsements(ctx)
	}
	// Stop before the independent runtime: the contract under test is evidence
	// acquisition, not the final EOF callbacks (which have their own deadline).
	input.NextEndorsements = func(ctx context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		check(ctx)
		return redaction.PageLayout{}, nil, io.ErrUnexpectedEOF
	}
	require.ErrorIs(t, VerifyFresh(t.Context(), bytes.NewReader(original), int64(len(original)), input), io.ErrUnexpectedEOF)
}

func TestCanceledEngineAcquireDoesNotWaitForActivePage(t *testing.T) {
	recipe := qualificationRecipe()
	engine, err := NewPDFium(recipe)
	require.NoError(t, err)
	e, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- e.withInstance(t.Context(), func(pdfium.Pdfium) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("active runtime did not enter: %v", err)
	case <-time.After(time.Duration(recipe.PageTimeoutSeconds)*time.Second + 30*time.Second):
		t.Fatal("active runtime did not start")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() { second <- e.withInstance(ctx, func(pdfium.Pdfium) error { return nil }) }()
	var secondErr error
	select {
	case secondErr = <-second:
		close(release)
	case <-time.After(100 * time.Millisecond):
		close(release)
		secondErr = <-second
		t.Error("canceled engine acquisition blocked behind an active page")
	}
	require.ErrorIs(t, secondErr, context.Canceled)
	require.NoError(t, <-finished)
}

func TestEvidenceBoundsPrecedeCanonicalMarshal(t *testing.T) {
	for _, endorsements := range [][]redaction.Endorsement{
		make([]redaction.Endorsement, 4097),
		{{Text: strings.Repeat("x", maxTextBytes+1)}},
		{{Kind: strings.Repeat("x", maxTextBytes+1)}},
		{{Box: redaction.Box{FrameSHA256: strings.Repeat("x", maxTextBytes+1)}}},
	} {
		require.ErrorContains(t, preflightEndorsements(endorsements), "bounds")
	}
	a := unicodeArtifact(t)
	a.Endorsements = []redaction.Endorsement{{Kind: strings.Repeat("x", maxTextBytes+1), Text: "public", FontSHA256: fontSHA256, Color: "#000000", Box: a.Runs[0].Boxes[0], FontSizeMilliPoints: 10000}}
	encoded, err := canonical.Marshal(a.Endorsements)
	require.NoError(t, err)
	a.EndorsementsSHA256 = digest(encoded)
	require.ErrorContains(t, validateArtifact(a, qualificationRecipe()), "bounds")
}

func TestVerifyIndependentEndorsementsArePreflighted(t *testing.T) {
	original, a := freshFixture(t)
	input := verificationFor([]PageArtifact{a})
	input.NextEndorsements = func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
		return a.Layout, []redaction.Endorsement{{Kind: strings.Repeat("x", maxTextBytes+1)}}, nil
	}
	require.ErrorContains(t, VerifyFresh(t.Context(), bytes.NewReader(original), int64(len(original)), input), "bounds")
}

func TestVerifyDeadlineInterruptsInFlightEvidence(t *testing.T) {
	original, a := freshFixture(t)
	for _, stage := range []string{"artifact", "text", "endorsements"} {
		t.Run(stage, func(t *testing.T) {
			input := verificationFor([]PageArtifact{a})
			wait := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			switch stage {
			case "artifact":
				input.NextPageArtifact = func(ctx context.Context) (PageArtifact, error) { return PageArtifact{}, wait(ctx) }
			case "text":
				input.NextText = func(ctx context.Context) (int, []byte, error) { return 0, nil, wait(ctx) }
			case "endorsements":
				input.NextEndorsements = func(ctx context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
					return redaction.PageLayout{}, nil, wait(ctx)
				}
			}
			recipe := qualificationRecipe()
			recipe.PageTimeoutSeconds = 1
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			start := time.Now()
			require.ErrorIs(t, verifyFresh(ctx, bytes.NewReader(original), int64(len(original)), input, recipe), context.DeadlineExceeded)
			require.Less(t, time.Since(start), 2*time.Second, "evidence must stop at its page deadline, not the later caller deadline")
		})
	}
}

func TestVerifyCancellationDuringPNGReadClosesReader(t *testing.T) {
	original, a := freshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := false
	input := verificationFor([]PageArtifact{a})
	next := input.NextPageArtifact
	input.NextPageArtifact = func(pageCtx context.Context) (PageArtifact, error) {
		artifact, err := next(pageCtx)
		artifact.OpenPNG = func() (io.ReadCloser, error) {
			return &cancelPNGReader{ctx: pageCtx, cancel: cancel, closed: &closed}, nil
		}
		return artifact, err
	}
	require.ErrorIs(t, VerifyFresh(ctx, bytes.NewReader(original), int64(len(original)), input), context.Canceled)
	require.True(t, closed)
}

type cancelPNGReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	closed *bool
}

func (r *cancelPNGReader) Read([]byte) (int, error) {
	r.cancel()
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r *cancelPNGReader) Close() error { *r.closed = true; return nil }

func TestFreshSnapshotIdentityAndReadOnlyHandle(t *testing.T) {
	data := []byte("synthetic final PDF snapshot")
	snapshot, err := freezeFresh(t.Context(), bytes.NewReader(data), int64(len(data)), 1000)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	require.Equal(t, digest(data), snapshot.sha256)
	require.Equal(t, int64(len(data)), snapshot.size)
	_, err = snapshot.file.WriteAt([]byte("changed"), 0)
	require.Error(t, err, "verification may hold only a read-only snapshot handle")
}

func TestPDFiumEnginesSharePinnedCompilationCache(t *testing.T) {
	first, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	e1, ok := first.(*pdfiumEngine)
	require.True(t, ok)
	e2, ok := second.(*pdfiumEngine)
	require.True(t, ok)
	require.Same(t, e1.cache, e2.cache, "sequential engines must not retire and reallocate compiled pinned code")
	require.NoError(t, first.Close())
	require.NoError(t, e2.withInstance(t.Context(), func(pdfium.Pdfium) error { return nil }), "closing one engine must not invalidate the package-owned code cache")
}

func TestFreshCMapRejectsDuplicateUnicodeDestinations(t *testing.T) {
	_, err := parseFreshCMap(t.Context(), []byte(cmapPrefix+"2 beginbfchar\n<0001> <0061>\n<0002> <0061>\nendbfchar\n"+cmapSuffix))
	require.Error(t, err, "the writer assigns exactly one CID to each Unicode scalar")
}

func TestFreshEnvelopeRejectsXrefAndObjectBoundaryAttacks(t *testing.T) {
	original, a := freshFixture(t)
	for _, test := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"trailing bytes", func(data []byte) []byte { return append(data, []byte("SYNTHETIC PRIVATE SOURCE")...) }},
		{"old revision", func(data []byte) []byte { return append(bytes.Clone(data), data...) }},
		{"free entry", func(data []byte) []byte {
			start := bytes.LastIndex(data, []byte("\nstream\n")) + 8
			data[start+9] = 0
			return data
		}},
		{"self reference", func(data []byte) []byte {
			start := bytes.LastIndex(data, []byte("\nstream\n")) + 8
			data[start+13*11+8] ^= 1
			return data
		}},
		{"generation", func(data []byte) []byte {
			start := bytes.LastIndex(data, []byte("\nstream\n")) + 8
			data[start+11+10] = 1
			return data
		}},
		{"xref dictionary key", func(data []byte) []byte {
			return bytes.Replace(data, []byte("/Type /XRef /Size"), []byte("/Prev 0 /Type /XRef /Size"), 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := test.edit(bytes.Clone(original))
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(data), int64(len(data)), verificationFor([]PageArtifact{a})))
		})
	}
}

func FuzzFreshEnvelopeAndStreams(f *testing.F) {
	for _, data := range [][]byte{[]byte(freshHeader), []byte("1 0 obj\n<<  /Length 3 >>\nstream\nabc\nendstream\nendobj\n"), []byte("startxref\n9999999999999999999999\n%%EOF\n"), bytes.Repeat([]byte{'x'}, 256)} {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFreshTextObject {
			t.Skip()
		}
		reader := bytes.NewReader(data)
		_, _ = parseFreshIndex(t.Context(), reader, int64(len(data)), 1)
		index := &freshIndex{reader: reader, offsets: []int64{0, 0}, ends: []int64{0, int64(len(data))}}
		_, _ = index.stream(t.Context(), 1, "")
	})
}
