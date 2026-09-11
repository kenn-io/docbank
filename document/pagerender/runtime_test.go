package pagerender

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media/mediatest"
)

func testSource(data []byte) document.PageSource {
	hash := sha256.Sum256(data)
	return document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
}

func syntheticPDF(pages []string, parent string) []byte {
	var kids bytes.Buffer
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for i, p := range pages {
		fmt.Fprintf(&kids, "%d 0 R ", i+3)
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /Resources << >> "+p+" >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d %s >>", kids.String(), len(pages), parent)
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	var offsets []int
	for i, o := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, n := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", n)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func pngChunk(kind string, data []byte) []byte {
	out := make([]byte, len(data)+12)
	binary.BigEndian.PutUint32(out, uint32(len(data)))
	copy(out[4:8], kind)
	copy(out[8:], data)
	binary.BigEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[4:len(out)-4]))
	return out
}
func densityPNG(ppm uint32) []byte {
	data := mediatest.PNG(254, 508, color.White)
	density := make([]byte, 9)
	binary.BigEndian.PutUint32(density, ppm)
	binary.BigEndian.PutUint32(density[4:], ppm)
	density[8] = 1
	out := append([]byte{}, data[:33]...)
	out = append(out, pngChunk("pHYs", density)...)
	return append(out, data[33:]...)
}

func TestInspectorPreservesInheritedCropAndRejectsUnsupportedGeometry(t *testing.T) {
	data := syntheticPDF([]string{"/MediaBox [-20 -30 220 330]"}, "/MediaBox [-10 -20 210 320] /CropBox [10.125 20.25 170.625 250.75] /Rotate 90")
	frames, err := InspectSource(data, testSource(data), "application/pdf")
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.Equal(t, [4]int64{101250, 202500, 1706250, 2507500}, frames[0].CropBox)
	require.Equal(t, 90, frames[0].Rotation)
	for _, extra := range []string{"/UserUnit 2", "/Rotate 45", "/CropBox [-1 0 1000 1000]"} {
		data = syntheticPDF([]string{extra}, "/MediaBox [0 0 612 792]")
		_, err = InspectSource(data, testSource(data), "application/pdf")
		require.Error(t, err)
	}
}

func TestPNGInspectionRequiresPhysicalDensityAndRejectsOrientation(t *testing.T) {
	data := densityPNG(10000)
	frames, err := InspectSource(data, testSource(data), "image/png")
	require.NoError(t, err)
	require.Equal(t, int64(10000), frames[0].Width)
	plain := mediatest.PNG(2, 2, color.White)
	_, err = InspectSource(plain, testSource(plain), "image/png")
	require.Error(t, err)
	oriented := append([]byte{}, data[:33]...)
	oriented = append(oriented, pngChunk("eXIf", []byte("synthetic"))...)
	oriented = append(oriented, data[33:]...)
	_, err = InspectSource(oriented, testSource(oriented), "image/png")
	require.Error(t, err)
}

func TestVerifyPNGRejectsWrongDimensionsAndCorruptPayload(t *testing.T) {
	data := mediatest.PNG(2, 3, color.White)
	source := testSource(data)
	receipt := document.PageImageV1{Contract: document.PageImageContractV1, Source: source, Page: 1, FrameSHA256: source.SHA256, RecipeSHA256: source.SHA256, SHA256: source.SHA256, Size: int64(len(data)), Width: 2, Height: 3}
	require.NoError(t, VerifyPNG(data, receipt))
	receipt.Width = 3
	require.Error(t, VerifyPNG(data, receipt))
	receipt.Width = 2
	data[len(data)-1] ^= 1
	require.Error(t, VerifyPNG(data, receipt))
	receipt.SHA256 = testSource(data).SHA256
	require.Error(t, VerifyPNG(data, receipt), "matching receipt hash cannot make corrupt PNG chunks valid")
}

func testPin(t *testing.T, path string) Executable {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	b, err := os.ReadFile(resolved)
	require.NoError(t, err)
	h := sha256.Sum256(b)
	return Executable{Path: resolved, SHA256: hex.EncodeToString(h[:])}
}

func TestRealRuntimeInspectAndRenderPDFAndPNG(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("optional renderer is Linux amd64 only")
	}
	renderer, err := exec.LookPath("pdftoppm")
	if err != nil {
		t.Skip("Poppler unavailable")
	}
	limiter, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit unavailable")
	}
	inspector := filepath.Join(t.TempDir(), "page-inspect")
	command := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", inspector, "./cmd/docbank-page-inspect")
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
	engine, err := New(t.Context(), Profile{Inspector: testPin(t, inspector), Renderer: testPin(t, renderer), Limiter: testPin(t, limiter), DeploymentIdentity: "synthetic-test-deployment"})
	require.NoError(t, err)
	data := syntheticPDF([]string{"/MediaBox [0 0 612 792]", "/MediaBox [0 0 595.2756 841.8898]", "/MediaBox [0 0 792 612]", "/MediaBox [-10 -20 210 320] /CropBox [10.125 20.25 170.625 250.75] /Rotate 90"}, "")
	source := testSource(data)
	frames, err := engine.Inspect(t.Context(), data, source, "application/pdf")
	require.NoError(t, err)
	require.Len(t, frames, 4)
	for i, want := range [][2]int64{{1224, 1584}, {1191, 1684}, {1584, 1224}, {461, 321}} {
		recipe, err := engine.Recipe(frames[i], 144)
		require.NoError(t, err)
		image, pixels, err := engine.Render(t.Context(), data, frames[i], recipe)
		require.NoError(t, err)
		require.Equal(t, want[0], image.Width)
		require.Equal(t, want[1], image.Height)
		require.NotEmpty(t, pixels)
	}
	data = densityPNG(10000)
	source = testSource(data)
	frames, err = engine.Inspect(t.Context(), data, source, "image/png")
	require.NoError(t, err)
	recipe, err := engine.Recipe(frames[0], 0)
	require.NoError(t, err)
	require.InDelta(t, 254.0, recipe.DPI, 0)
	_, pixels, err := engine.Render(t.Context(), data, frames[0], recipe)
	require.NoError(t, err)
	require.Equal(t, data, pixels)
	_, err = engine.Recipe(frames[0], 144)
	require.Error(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = engine.Inspect(canceled, data, source, "image/png")
	require.ErrorIs(t, err, context.Canceled)
	wrongSource := source
	wrongSource.Size++
	_, err = engine.Inspect(t.Context(), data, wrongSource, "image/png")
	require.Error(t, err)
	malformed := []byte("%PDF-1.7\nmalformed synthetic source")
	_, err = engine.Inspect(t.Context(), malformed, testSource(malformed), "application/pdf")
	require.Error(t, err)
	probe := filepath.Join(t.TempDir(), "resource-probe")
	command = exec.CommandContext(t.Context(), "go", "build", "-o", probe, "./testdata/resource-probe")
	out, err = command.CombinedOutput()
	require.NoError(t, err, string(out))
	pin := testPin(t, probe)
	compiled, err := providerutil.LoadPinnedExecutable(pin.Path, pin.SHA256, 64<<20)
	require.NoError(t, err)
	_, _, err = engine.command(t.Context(), compiled, nil, 2<<30, 1024)
	require.ErrorIs(t, err, ErrUnsupported, "3 GiB allocation must fail under the actual 2 GiB pre-exec limit")
	timeout, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	_, _, err = engine.command(timeout, compiled, nil, 2<<30, 1024, "wait")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
