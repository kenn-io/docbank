package docxpdf

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

const testRuntimeIdentity = "LibreOffice synthetic renderer"

var testHelperBinary string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "docbank-docxpdf-test-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	testHelperBinary = filepath.Join(directory, "renderer-base"+extension)
	command := exec.Command("go", "build", "-o", testHelperBinary, "./testdata/renderer")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build DOCX PDF test helper: %v\n%s", buildErr, output)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func TestConvertReturnsVerifiedReceipt(t *testing.T) {
	executable := helperExecutable(t, "ok")
	policy := policyFor(t, executable, DefaultLimits())
	source, reader := sourceFor(t, syntheticDOCX())
	result, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	require.True(t, reader.closed)
	receipt := result.Receipt()
	require.Equal(t, source.SHA256, receipt.SourceSHA256)
	require.Equal(t, source.Size, receipt.SourceBytes)
	require.Equal(t, digest(result.PDF()), receipt.PDFSHA256)
	require.Equal(t, int64(len(result.PDF())), receipt.PDFBytes)
	require.Equal(t, 3, receipt.Pages)
	require.Equal(t, policy.Fingerprint(), receipt.PolicyFingerprint)
	require.Equal(t, ConverterVersion, receipt.ConverterVersion)
	pdfSource, err := result.Source()
	require.NoError(t, err)
	require.Equal(t, receipt.PDFSHA256, pdfSource.SHA256)
	require.Equal(t, receipt.PDFBytes, pdfSource.Size)
	require.NoError(t, pdfSource.Content.Close())
}

func TestConvertRunsRendererWithPrivateProfileAndCleanEnvironment(t *testing.T) {
	t.Setenv("DOCBANK_DOCXPDF_AMBIENT_SECRET", "synthetic-secret")
	t.Setenv("MISTRAL_API_KEY", "synthetic-key")
	policy := policyFor(t, helperExecutable(t, "ok"), DefaultLimits())
	source, _ := sourceFor(t, syntheticDOCX())
	result, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	require.Equal(t, 3, result.Receipt().Pages)
}

func TestConvertRejectsFailedRendering(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      string
		limits    func(Limits) Limits
		wantError string
	}{
		{name: "none", mode: "none", wantError: "DOCX renderer produced no PDF"},
		{name: "fail", mode: "fail", wantError: "DOCX renderer failed"},
		{name: "notpdf", mode: "notpdf", wantError: "verify generated PDF"},
		{name: "zero", mode: "zero", wantError: "verify generated PDF"},
		{name: "dir", mode: "dir", wantError: "DOCX renderer produced no PDF"},
		{name: "size:MaxPDFBytes+1", mode: "size", limits: func(l Limits) Limits { l.MaxPDFBytes = 1024; return l }, wantError: "generated PDF exceeds byte limit"},
		{name: "pages:MaxPages+1", mode: "pages", limits: func(l Limits) Limits { l.MaxPages = 3; return l }, wantError: "generated PDF exceeds page limit or has no pages"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := conversionDirectories(t)
			limits := DefaultLimits()
			if test.limits != nil {
				limits = test.limits(limits)
			}
			t.Logf("MaxPDFBytes=%d MaxPages=%d", limits.MaxPDFBytes, limits.MaxPages)
			policy := policyFor(t, helperExecutable(t, test.mode), limits)
			source, _ := sourceFor(t, syntheticDOCX())
			result, err := Convert(t.Context(), source, policy)
			require.Nil(t, result)
			require.ErrorContains(t, err, test.wantError)
			require.Equal(t, before, conversionDirectories(t))
		})
	}
}

func TestConvertAcceptsExactLimitsAndRendererNoise(t *testing.T) {
	exactPDFBytes := int64(len(syntheticPDF(3)))
	limits := DefaultLimits()
	limits.MaxPDFBytes = exactPDFBytes
	limits.MaxPages = 3
	policy := policyFor(t, helperExecutable(t, "noise"), limits)
	source, _ := sourceFor(t, syntheticDOCX())
	result, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	t.Logf("MaxPDFBytes=%d MaxPages=%d", limits.MaxPDFBytes, limits.MaxPages)
	require.Equal(t, limits.MaxPDFBytes, result.Receipt().PDFBytes, "MaxPDFBytes")
	require.Equal(t, limits.MaxPages, result.Receipt().Pages, "MaxPages")
}

func TestConvertRejectsUnverifiedSourceBeforeLaunch(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ocr.Source, []byte)
		want string
	}{
		{name: "size plus one", edit: func(source *ocr.Source, _ []byte) { source.Size++ }, want: "does not match declared size"},
		{name: "size minus one", edit: func(source *ocr.Source, _ []byte) { source.Size-- }, want: "does not match declared size"},
		{name: "sha", edit: func(source *ocr.Source, _ []byte) { source.SHA256 = strings.Repeat("0", 64) }, want: "does not match declared size"},
		{name: "media type", edit: func(source *ocr.Source, _ []byte) { source.MediaType = "application/pdf" }, want: "requires the Word media type"},
		{name: "PPTX ZIP", edit: func(source *ocr.Source, data []byte) {
			replaceSource(source, data, syntheticOOXML("ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"))
		}, want: "not a Word document"},
		{name: "DOCX-shaped ZIP without override", edit: func(source *ocr.Source, data []byte) {
			replaceSource(source, data, syntheticZIP(map[string]string{"word/document.xml": "<document/>", "[Content_Types].xml": "<Types xmlns=\"http://schemas.openxmlformats.org/package/2006/content-types\"></Types>"}))
		}, want: "not a Word document"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := syntheticDOCX()
			source, _ := sourceFor(t, data)
			test.edit(&source, data)
			policy := policyFor(t, helperExecutable(t, "ok"), DefaultLimits())
			result, err := Convert(t.Context(), source, policy)
			require.Nil(t, result)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestConvertClosesSourceOnEveryPath(t *testing.T) {
	executable := helperExecutable(t, "ok")
	policy := policyFor(t, executable, DefaultLimits())
	t.Run("success", func(t *testing.T) {
		source, reader := sourceFor(t, syntheticDOCX())
		result, err := Convert(t.Context(), source, policy)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 1, reader.closeCount)
	})
	t.Run("read error", func(t *testing.T) {
		data := syntheticDOCX()
		reader := &trackedReader{Reader: &failAfterReader{data: data, split: len(data) / 2}}
		source := ocr.Source{Content: reader, MediaType: docxMediaType, Size: int64(len(data)), SHA256: digest(data)}
		result, err := Convert(t.Context(), source, policy)
		require.Nil(t, result)
		require.ErrorContains(t, err, "read DOCX source failed")
		require.Equal(t, 1, reader.closeCount)
	})
	t.Run("close error", func(t *testing.T) {
		source, reader := sourceFor(t, syntheticDOCX())
		reader.closeErr = errors.New("synthetic close error")
		result, err := Convert(t.Context(), source, policy)
		require.Nil(t, result)
		require.EqualError(t, err, "close DOCX source failed")
		require.Equal(t, 1, reader.closeCount)
	})
}

func TestConvertCancellationAndTimeoutStopRenderer(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		before := conversionDirectories(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		policy := policyFor(t, helperExecutable(t, "hang"), DefaultLimits())
		source, _ := sourceFor(t, syntheticDOCX())
		resultChannel := make(chan conversionResult, 1)
		go func() {
			result, err := Convert(ctx, source, policy)
			resultChannel <- conversionResult{result: result, err: err}
		}()
		waitForStarted(t)
		cancel()
		result := <-resultChannel
		require.Nil(t, result.result)
		require.ErrorIs(t, result.err, context.Canceled)
		require.Equal(t, before, conversionDirectories(t))
	})
	t.Run("timeout", func(t *testing.T) {
		before := conversionDirectories(t)
		limits := DefaultLimits()
		limits.Timeout = 200 * time.Millisecond
		policy := policyFor(t, helperExecutable(t, "hang"), limits)
		source, _ := sourceFor(t, syntheticDOCX())
		result, err := Convert(t.Context(), source, policy)
		require.Nil(t, result)
		require.EqualError(t, err, "DOCX renderer timed out")
		require.Equal(t, before, conversionDirectories(t))
	})
	if runtime.GOOS == "windows" {
		t.Run("grandchild", func(t *testing.T) {
			before := conversionDirectories(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			policy := policyFor(t, helperExecutable(t, "tree"), DefaultLimits())
			source, _ := sourceFor(t, syntheticDOCX())
			resultChannel := make(chan conversionResult, 1)
			go func() {
				result, err := Convert(ctx, source, policy)
				resultChannel <- conversionResult{result: result, err: err}
			}()
			waitForStarted(t)
			cancel()
			result := <-resultChannel
			require.Nil(t, result.result)
			require.ErrorIs(t, result.err, context.Canceled)
			require.Equal(t, before, conversionDirectories(t))
		})
	}
}

func TestConvertHandlesLibreOfficeNormalRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix LibreOffice direct binaries use the normal restart exit code")
	}
	t.Run("restart once", func(t *testing.T) {
		policy := policyFor(t, helperExecutable(t, "restart"), DefaultLimits())
		source, _ := sourceFor(t, syntheticDOCX())
		result, err := Convert(t.Context(), source, policy)
		require.NoError(t, err)
		require.Equal(t, 3, result.Receipt().Pages)
	})
	t.Run("repeated restart fails", func(t *testing.T) {
		policy := policyFor(t, helperExecutable(t, "restart-always"), DefaultLimits())
		source, _ := sourceFor(t, syntheticDOCX())
		result, err := Convert(t.Context(), source, policy)
		require.Nil(t, result)
		require.EqualError(t, err, "DOCX renderer failed")
	})
}

func TestPolicyAndConvertEnforceRendererPin(t *testing.T) {
	validExecutable := helperExecutable(t, "ok")
	validSHA := executableSHA256(t, validExecutable)
	limits := DefaultLimits()
	for _, test := range []struct {
		name string
		edit func(*Renderer)
	}{
		{name: "relative path", edit: func(renderer *Renderer) { renderer.Executable = "renderer" }},
		{name: "unclean path", edit: func(renderer *Renderer) {
			renderer.Executable = validExecutable + string(os.PathSeparator) + "."
		}},
		{name: "wrong SHA", edit: func(renderer *Renderer) { renderer.ExecutableSHA256 = strings.Repeat("0", 64) }},
		{name: "missing file", edit: func(renderer *Renderer) {
			renderer.Executable = filepath.Join(filepath.Dir(validExecutable), "missing-renderer")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			renderer := Renderer{Executable: validExecutable, ExecutableSHA256: validSHA, RuntimeIdentity: testRuntimeIdentity}
			test.edit(&renderer)
			_, err := NewPolicy(renderer, limits)
			require.Error(t, err)
		})
	}
	if runtime.GOOS != "windows" {
		t.Run("symlink", func(t *testing.T) {
			link := filepath.Join(t.TempDir(), "renderer-link")
			require.NoError(t, os.Symlink(validExecutable, link))
			_, err := NewPolicy(Renderer{Executable: link, ExecutableSHA256: validSHA, RuntimeIdentity: testRuntimeIdentity}, limits)
			require.Error(t, err)
		})
	}
	t.Run("replacement after policy", func(t *testing.T) {
		executable := helperExecutable(t, "ok")
		data, err := os.ReadFile(executable)
		require.NoError(t, err)
		policy, err := NewPolicy(Renderer{Executable: executable, ExecutableSHA256: executableSHA256(t, executable), RuntimeIdentity: testRuntimeIdentity}, limits)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(executable, append(data, []byte("replacement")...), 0o700))
		source, _ := sourceFor(t, syntheticDOCX())
		result, err := Convert(t.Context(), source, policy)
		require.Nil(t, result)
		require.EqualError(t, err, "DOCX renderer identity changed")
	})
}

func TestNewPolicyValidatesLimitsAndFingerprint(t *testing.T) {
	executable := helperExecutable(t, "ok")
	renderer := Renderer{Executable: executable, ExecutableSHA256: executableSHA256(t, executable), RuntimeIdentity: testRuntimeIdentity}
	defaults := DefaultLimits()
	base, err := NewPolicy(renderer, defaults)
	require.NoError(t, err)
	require.NotEmpty(t, base.Fingerprint())
	require.Empty(t, (Policy{}).Fingerprint())
	for _, test := range []struct {
		name string
		edit func(*Limits)
	}{
		{name: "source zero", edit: func(limits *Limits) { limits.MaxSourceBytes = 0 }},
		{name: "source ceiling plus one", edit: func(limits *Limits) { limits.MaxSourceBytes = defaults.MaxSourceBytes + 1 }},
		{name: "pdf zero", edit: func(limits *Limits) { limits.MaxPDFBytes = 0 }},
		{name: "pdf ceiling plus one", edit: func(limits *Limits) { limits.MaxPDFBytes = defaults.MaxPDFBytes + 1 }},
		{name: "pages zero", edit: func(limits *Limits) { limits.MaxPages = 0 }},
		{name: "pages ceiling plus one", edit: func(limits *Limits) { limits.MaxPages = defaults.MaxPages + 1 }},
		{name: "timeout zero", edit: func(limits *Limits) { limits.Timeout = 0 }},
		{name: "timeout ceiling plus one", edit: func(limits *Limits) { limits.Timeout = defaults.Timeout + time.Nanosecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := defaults
			test.edit(&limits)
			_, err := NewPolicy(renderer, limits)
			require.Error(t, err)
		})
	}
	changedRuntime := renderer
	changedRuntime.RuntimeIdentity += " changed"
	changed, err := NewPolicy(changedRuntime, defaults)
	require.NoError(t, err)
	require.NotEqual(t, base.Fingerprint(), changed.Fingerprint())
	changedLimits := defaults
	changedLimits.MaxPages--
	changed, err = NewPolicy(renderer, changedLimits)
	require.NoError(t, err)
	require.NotEqual(t, base.Fingerprint(), changed.Fingerprint())
	changedExecutable := helperExecutable(t, "pin-change")
	require.NoError(t, os.WriteFile(changedExecutable, append(mustRead(t, changedExecutable), []byte("identity")...), 0o700))
	changed, err = NewPolicy(Renderer{Executable: changedExecutable, ExecutableSHA256: executableSHA256(t, changedExecutable), RuntimeIdentity: testRuntimeIdentity}, defaults)
	require.NoError(t, err)
	require.NotEqual(t, base.Fingerprint(), changed.Fingerprint())
	marker := filepath.Join(t.TempDir(), "must-not-launch")
	noLaunchExecutable := helperExecutable(t, "no-launch")
	_, err = NewPolicy(Renderer{Executable: noLaunchExecutable, ExecutableSHA256: executableSHA256(t, noLaunchExecutable), RuntimeIdentity: marker}, defaults)
	require.NoError(t, err)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
	source, _ := sourceFor(t, syntheticDOCX())
	result, err := Convert(t.Context(), source, Policy{})
	require.Nil(t, result)
	require.EqualError(t, err, "DOCX PDF policy is invalid; use NewPolicy")
}

func TestResultAccessorsReturnCopies(t *testing.T) {
	executable := helperExecutable(t, "ok")
	policy := policyFor(t, executable, DefaultLimits())
	source, _ := sourceFor(t, syntheticDOCX())
	result, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	wantPDF := result.PDF()
	pdf := result.PDF()
	pdf[0] = 0
	require.Equal(t, wantPDF, result.PDF())
	receipt := result.Receipt()
	receipt.Pages = 99
	receipt.PDFSHA256 = "changed"
	require.Equal(t, 3, result.Receipt().Pages)
	require.Equal(t, digest(wantPDF), result.Receipt().PDFSHA256)
	for _, zero := range []*Result{nil, {}} {
		require.Empty(t, zero.PDF())
		require.Empty(t, zero.Receipt())
		_, err := zero.Source()
		require.EqualError(t, err, "DOCX PDF result is invalid")
	}
}

type conversionResult struct {
	result *Result
	err    error
}

type trackedReader struct {
	io.Reader

	closed     bool
	closeCount int
	closeErr   error
}

func (reader *trackedReader) Close() error {
	reader.closed = true
	reader.closeCount++
	return reader.closeErr
}

func sourceFor(t *testing.T, data []byte) (ocr.Source, *trackedReader) {
	t.Helper()
	reader := &trackedReader{Reader: bytes.NewReader(data)}
	source, err := ocr.NewSource(reader, docxMediaType, int64(len(data)), digest(data))
	require.NoError(t, err)
	return source, reader
}

func policyFor(t *testing.T, executable string, limits Limits) Policy {
	t.Helper()
	policy, err := NewPolicy(Renderer{Executable: executable, ExecutableSHA256: executableSHA256(t, executable), RuntimeIdentity: testRuntimeIdentity}, limits)
	require.NoError(t, err)
	return policy
}

func helperExecutable(t *testing.T, mode string) string {
	t.Helper()
	extension := filepath.Ext(testHelperBinary)
	target := filepath.Join(t.TempDir(), "renderer-"+mode+extension)
	require.NoError(t, copyFile(testHelperBinary, target, 0o700))
	return target
}

func copyFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, mode)
}

func executableSHA256(t *testing.T, executable string) string {
	t.Helper()
	return digest(mustRead(t, executable))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func conversionDirectories(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	require.NoError(t, err)
	var directories []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "docbank-docxpdf-") && !strings.HasPrefix(entry.Name(), "docbank-docxpdf-test-") {
			directories = append(directories, entry.Name())
		}
	}
	return directories
}

func waitForStarted(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, directory := range conversionDirectories(t) {
			if _, err := os.Stat(filepath.Join(os.TempDir(), directory, "started")); err == nil {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("renderer did not start")
		case <-ticker.C:
		}
	}
}

func replaceSource(source *ocr.Source, _, replacement []byte) {
	reader := source.Content
	_ = reader.Close()
	*source = ocr.Source{Content: &trackedReader{Reader: bytes.NewReader(replacement)}, MediaType: docxMediaType, Size: int64(len(replacement)), SHA256: digest(replacement)}
}

type failAfterReader struct {
	data   []byte
	split  int
	offset int
}

func (reader *failAfterReader) Read(buffer []byte) (int, error) {
	if reader.offset >= reader.split {
		return 0, errors.New("synthetic reader failure")
	}
	end := min(reader.offset+len(buffer), reader.split)
	n := copy(buffer, reader.data[reader.offset:end])
	reader.offset += n
	return n, nil
}

func syntheticDOCX() []byte {
	return syntheticZIP(map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body/></w:document>`,
	})
}

func syntheticOOXML(name, contentType string) []byte {
	return syntheticZIP(map[string]string{
		"[Content_Types].xml": fmt.Sprintf(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/%s" ContentType="%s"/></Types>`, name, contentType),
		name:                  "<document/>",
	})
}

func syntheticZIP(entries map[string]string) []byte {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(entry, content)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return output.Bytes()
}

func syntheticPDF(pageCount int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", pageReferences(pageCount), pageCount),
	}
	for range pageCount {
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func pageReferences(pageCount int) string {
	references := make([]string, pageCount)
	for index := range pageCount {
		references[index] = fmt.Sprintf("%d 0 R", index+3)
	}
	return strings.Join(references, " ")
}
