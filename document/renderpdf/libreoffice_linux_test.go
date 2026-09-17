//go:build linux

package renderpdf

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingRunner struct {
	inner   Runner
	mu      sync.Mutex
	calls   []Request
	results []StageResult
}

func (runner *countingRunner) Identity() string { return runner.inner.Identity() }

func (runner *countingRunner) Run(ctx context.Context, request Request) (StageResult, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, request)
	runner.mu.Unlock()
	result, err := runner.inner.Run(ctx, request)
	runner.mu.Lock()
	runner.results = append(runner.results, result)
	runner.mu.Unlock()
	return result, err
}

func (runner *countingRunner) count() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return len(runner.calls)
}

func (runner *countingRunner) call(index int) Request {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.calls[index]
}

func TestLibreOfficeConvertsSafeDOCX(t *testing.T) {
	policy := realLibreOfficePolicy(t, nil)
	content := realDOCX(false, "", "")
	result, err := Convert(t.Context(), testSource(t, content, docxMediaType), "docx", policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.PDF())
	assert.Positive(t, result.Receipt().Pages)
}

func TestLibreOfficeColdProfileRestartsExactlyOnce(t *testing.T) {
	policy := realLibreOfficePolicy(t, nil)
	runner := &countingRunner{inner: policy.runner}
	policy.runner = runner
	result, err := Convert(t.Context(), testSource(t, realDOCX(false, "", ""), docxMediaType), "docx", policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	runner.mu.Lock()
	results := append([]StageResult(nil), runner.results...)
	runner.mu.Unlock()
	require.Len(t, results, 2)
	for _, stage := range results {
		assert.Equal(t, 1, stage.Attestation.RestartCount)
	}
	t.Logf("owner restart counts: normalize=%d pdf=%d", results[0].Attestation.RestartCount, results[1].Attestation.RestartCount)
	t.Log("cold-profile owner conversion completed through the launcher's single retry contract")
}

func TestLibreOfficeRejectsOrStripsExternalDOCXTargets(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	hit := make(chan struct{}, 1)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case hit <- struct{}{}:
			default:
			}
			_ = connection.Close()
		}
	}()
	httpTarget := "http://" + listener.Addr().String() + "/external.png"
	fileTarget := "file://" + filepath.Join(t.TempDir(), "host-sentinel")
	sentinelPath := strings.TrimPrefix(fileTarget, "file://")
	require.NoError(t, os.WriteFile(sentinelPath, []byte("host-only"), 0o600))
	policy := realLibreOfficePolicy(t, nil)
	runner := &countingRunner{inner: policy.runner}
	policy.runner = runner
	content := realDOCX(true, httpTarget, fileTarget)
	result, err := Convert(t.Context(), testSource(t, content, docxMediaType), "docx", policy)
	select {
	case <-hit:
		t.Fatal("LibreOffice reached the host listener")
	case <-time.After(100 * time.Millisecond):
	}
	contentAfter, readErr := os.ReadFile(sentinelPath)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("host-only"), contentAfter)
	if err == nil {
		require.NotNil(t, result)
		assert.Equal(t, 2, runner.count())
		stage := runner.call(1)
		normalized := string(stage.Input)
		assert.NotContains(t, normalized, "WEBSERVICE")
		assert.NotContains(t, normalized, httpTarget)
		assert.NotContains(t, normalized, fileTarget)
		assert.Equal(t, digest(stage.Input), stage.InputSHA256)
		t.Logf("admission branch: strip; stage calls=%d; normalized input sha256=%s", runner.count(), stage.InputSHA256)
	} else {
		assert.Nil(t, result)
		assert.Equal(t, 1, runner.count())
		t.Logf("admission branch: reject; stage calls=%d; listener hits=0; file sentinel=%q; error=%v", runner.count(), contentAfter, err)
	}
}

func TestLibreOfficeCancellationReapsProcessTree(t *testing.T) {
	policy := realLibreOfficePolicy(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := testSource(t, realDOCX(false, "", ""), docxMediaType)
	outcome := make(chan struct {
		result *Result
		err    error
	}, 1)
	go func() {
		result, err := Convert(ctx, source, "docx", policy)
		outcome <- struct {
			result *Result
			err    error
		}{result: result, err: err}
	}()
	pids := waitForLibreOfficeStart(t, policy.renderer.Executable)
	t.Logf("owner cancellation observed LibreOffice pids=%v", pids)
	cancel()
	finished := <-outcome
	result, err := finished.result, finished.err
	assert.Nil(t, result)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	waitForLibreOfficeExit(t, policy.renderer.Executable)
	assert.Empty(t, matchingLibreOfficePIDs(policy.renderer.Executable))
}

func waitForLibreOfficeStart(t *testing.T, executable string) map[int]struct{} {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pids := matchingLibreOfficePIDs(executable)
		if len(pids) != 0 {
			return pids
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("LibreOffice did not start: executable=%s", executable)
	return nil
}

func waitForLibreOfficeExit(t *testing.T, executable string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(matchingLibreOfficePIDs(executable)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("LibreOffice descendant survived cancellation: executable=%s pids=%v", executable, matchingLibreOfficePIDs(executable))
}

func matchingLibreOfficePIDs(executable string) map[int]struct{} {
	result := make(map[int]struct{})
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return result
	}
	base := filepath.Base(executable)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		path := filepath.Join("/proc", entry.Name())
		commandLine, err := os.ReadFile(filepath.Join(path, "cmdline"))
		if err != nil {
			continue
		}
		arguments := bytes.Split(commandLine, []byte{0})
		if len(arguments) > 0 && filepath.Base(string(arguments[0])) == base {
			result[pid] = struct{}{}
		}
	}
	return result
}

//nolint:unparam // the wrapper permits injected runners for owner variants.
func realLibreOfficePolicy(t *testing.T, runner Runner) Policy {
	t.Helper()
	executable := os.Getenv("DOCBANK_TEST_LIBREOFFICE_EXECUTABLE")
	if executable == "" {
		executable = "/usr/lib/libreoffice/program/soffice.bin"
	}
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	manifest, err := DiscoverRuntime(DefaultRuntimeRoots())
	require.NoError(t, err)
	if runner == nil {
		runner, err = newNativeRunner(Renderer{Runtime: manifest.Files, RuntimeSymlinks: manifest.Symlinks, RuntimeIdentity: manifest.Identity})
		require.NoError(t, err)
	}
	policy, err := NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runtime: manifest.Files, RuntimeSymlinks: manifest.Symlinks,
		RuntimeIdentity: manifest.Identity, Runner: runner,
	}, DefaultLimits())
	require.NoError(t, err)
	return policy
}

func realDOCX(external bool, httpTarget, fileTarget string) []byte {
	entries := map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body><w:p><w:r><w:t>safe synthetic document</w:t></w:r></w:p>`,
	}
	if external {
		entries["word/document.xml"] += `<w:p><w:r><w:drawing><wp:inline><wp:extent cx="1" cy="1"/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:blipFill><a:blip r:embed="rId2"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p><w:p><w:r><w:drawing><wp:inline><wp:extent cx="1" cy="1"/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:blipFill><a:blip r:embed="rId3"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p><w:p><w:fldSimple w:instr="WEBSERVICE(&quot;` + httpTarget + `&quot;)"><w:r><w:t>external field</w:t></w:r></w:fldSimple></w:p>`
		entries["word/_rels/document.xml.rels"] = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="` + httpTarget + `" TargetMode="External"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="` + fileTarget + `" TargetMode="External"/></Relationships>`
	}
	entries["word/document.xml"] += `</w:body></w:document>`
	return zipEntries(entries)
}

func zipEntries(entries map[string]string) []byte {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range entries {
		file, err := archive.Create(name)
		if err != nil {
			return nil
		}
		if _, err := io.WriteString(file, content); err != nil {
			return nil
		}
	}
	if err := archive.Close(); err != nil {
		return nil
	}
	return buffer.Bytes()
}
