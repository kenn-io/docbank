package pymupdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/docbank/document/isolate"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

const testRunnerIdentity = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestNewRequiresPinnedIsolatedRunnerAndExecutableDigest(t *testing.T) {
	executable := helperExecutable(t, "complete")
	executableBytes, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(executableBytes)
	profile := Profile{
		Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
		RuntimeIdentity: testRuntimeIdentity, Runner: &recordingRunner{identity: testRunnerIdentity},
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxPages: 10, Timeout: time.Second,
	}

	_, err = New(profile)
	require.NoError(t, err)

	profile.Runner = nil
	provider, err := New(profile)
	if runtime.GOOS == "linux" {
		require.NoError(t, err)
		require.NotNil(t, provider.runner)
	} else {
		require.ErrorIs(t, err, isolate.ErrIsolationUnavailable)
	}

	profile.Runner = &recordingRunner{identity: "mutable-latest"}
	_, err = New(profile)
	require.ErrorContains(t, err, "runner identity")

	profile.Runner = &recordingRunner{identity: testRunnerIdentity}
	profile.ExecutableSHA256 = ""
	_, err = New(profile)
	require.ErrorContains(t, err, "executable SHA-256")

	profile.ExecutableSHA256 = hex.EncodeToString(digest[:])
	profile.RuntimeIdentity = ""
	_, err = New(profile)
	require.ErrorContains(t, err, "runtime identity")
}

func TestProviderDelegatesOnlyAnExactFailClosedIsolationRequest(t *testing.T) {
	t.Setenv("DOCBANK_PYMUPDF_AMBIENT_SECRET", "must-not-reach-runner")
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	source := testPDF(2)
	upload := newTestUpload(source)

	_, err = document.RenderRendition(t.Context(), provider, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.NotNil(t, runner.request, "provider bypassed the isolated runner")
	assert.Equal(t, executable, runner.request.Executable)
	assert.Equal(t, profile.ExecutableSHA256, runner.request.ExecutableSHA256)
	assert.Equal(t, []string{"--protocol", protocolVersion}, runner.request.Arguments)
	expectedEnvironment := []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "PYTHONHASHSEED=0",
		"PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1",
	}
	assert.Equal(t, expectedEnvironment, runner.request.Environment)
	assert.NotContains(t, strings.Join(runner.request.Environment, "\n"), "must-not-reach-runner")
	assert.Equal(t, filepath.Dir(executable), runner.request.Directory)
	assert.Equal(t, source, runner.request.Stdin)
	stdinDigest := sha256.Sum256(source)
	assert.Equal(t, hex.EncodeToString(stdinDigest[:]), runner.request.StdinSHA256)
	assert.Equal(t, int64(1<<20), runner.request.MaxStdoutBytes)
	assert.Equal(t, isolate.IsolationRequirements{
		NetworkDisabled: true, KillProcessTree: true, VerifyExecutableSHA256: true,
	}, runner.request.Requirements)
	require.Len(t, runner.request.PolicyFingerprint, 64)
}

func TestProviderRejectsIncompleteOrMismatchedIsolationAttestation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*isolate.IsolationAttestation)
	}{
		{name: "runner identity", mutate: func(value *isolate.IsolationAttestation) { value.RunnerIdentity = testRuntimeIdentity }},
		{name: "policy", mutate: func(value *isolate.IsolationAttestation) { value.PolicyFingerprint = strings.Repeat("0", 64) }},
		{name: "executable", mutate: func(value *isolate.IsolationAttestation) { value.ExecutableSHA256 = strings.Repeat("0", 64) }},
		{name: "stdin", mutate: func(value *isolate.IsolationAttestation) { value.StdinSHA256 = strings.Repeat("0", 64) }},
		{name: "network", mutate: func(value *isolate.IsolationAttestation) { value.NetworkDisabled = false }},
		{name: "process tree", mutate: func(value *isolate.IsolationAttestation) { value.ProcessTreeContained = false }},
		{name: "digest launch", mutate: func(value *isolate.IsolationAttestation) { value.DigestVerifiedLaunch = false }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			executable := helperExecutable(t, "complete")
			runner := &recordingRunner{identity: testRunnerIdentity}
			runner.run = func(ctx context.Context, request isolate.IsolatedRunRequest) (isolate.IsolatedRunResult, error) {
				result, err := defaultRun(ctx, runner.identity, request)
				testCase.mutate(&result.Attestation)
				return result, err
			}
			profile := testProfile(t, executable, time.Second, 1<<20)
			profile.Runner = runner
			provider, err := New(profile)
			require.NoError(t, err)
			upload := newTestUpload(testPDF(2))

			_, err = provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
		})
	}
}

func TestProviderReverifiesExecutableAndRunnerIdentityBeforeEveryRun(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, executable string, runner *recordingRunner)
	}{
		{name: "executable replacement", mutate: func(t *testing.T, executable string, _ *recordingRunner) {
			t.Helper()
			require.NoError(t, os.WriteFile(executable, []byte("synthetic replacement"), 0o700))
		}},
		{name: "runner identity drift", mutate: func(_ *testing.T, _ string, runner *recordingRunner) {
			runner.identity = testRuntimeIdentity
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			executable := helperExecutable(t, "complete")
			runner := &recordingRunner{identity: testRunnerIdentity}
			profile := testProfile(t, executable, time.Second, 1<<20)
			profile.Runner = runner
			provider, err := New(profile)
			require.NoError(t, err)
			testCase.mutate(t, executable, runner)
			upload := newTestUpload(testPDF(2))

			_, err = provider.Render(t.Context(), upload,
				testAuthorization(provider.Descriptor(), upload.Metadata()))
			assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
			assert.Nil(t, runner.request)
		})
	}
}

func TestProviderFailsClosedWhenRunnerCannotEnforceIsolation(t *testing.T) {
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity, run: func(
		context.Context, isolate.IsolatedRunRequest,
	) (isolate.IsolatedRunResult, error) {
		return isolate.IsolatedRunResult{}, errors.Join(isolate.ErrIsolationUnavailable, errors.New("private-runner-detail"))
	}}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testPDF(2))

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	assert.NotContains(t, err.Error(), "private-runner-detail")
}

func TestProviderRequiresProcessTreeCleanupAttestationAfterRunnerCancellation(t *testing.T) {
	executable := helperExecutable(t, "complete")
	renderCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &recordingRunner{identity: testRunnerIdentity}
	runner.run = func(ctx context.Context, request isolate.IsolatedRunRequest) (isolate.IsolatedRunResult, error) {
		cancel()
		<-ctx.Done()
		result := isolatedResult(runner.identity, request, nil)
		result.Attestation.ProcessTreeContained = false
		return result, ctx.Err()
	}
	profile := testProfile(t, executable, time.Minute, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testPDF(2))

	_, err = provider.Render(renderCtx, upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NotNil(t, runner.request)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
}

func TestProviderConstrainsRunnerStdoutToAuthorizedTotalResultBytes(t *testing.T) {
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity, run: func(
		context.Context, isolate.IsolatedRunRequest,
	) (isolate.IsolatedRunResult, error) {
		return isolate.IsolatedRunResult{}, isolate.ErrIsolationUnavailable
	}}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testPDF(2))
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
	authorization.MaxTotalResultBytes = 128

	_, err = provider.Render(t.Context(), upload, authorization)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	require.NotNil(t, runner.request)
	assert.Equal(t, int64(128), runner.request.MaxStdoutBytes)
}

type recordingRunner struct {
	identity string
	request  *isolate.IsolatedRunRequest
	run      func(context.Context, isolate.IsolatedRunRequest) (isolate.IsolatedRunResult, error)
}

func (runner *recordingRunner) Identity() string { return runner.identity }

func (runner *recordingRunner) Run(ctx context.Context, request isolate.IsolatedRunRequest) (isolate.IsolatedRunResult, error) {
	copied := request
	copied.Arguments = append([]string(nil), request.Arguments...)
	copied.Environment = append([]string(nil), request.Environment...)
	copied.Stdin = append([]byte(nil), request.Stdin...)
	runner.request = &copied
	if runner.run != nil {
		return runner.run(ctx, request)
	}
	return defaultRun(ctx, runner.identity, request)
}

func defaultRun(ctx context.Context, runnerIdentity string, request isolate.IsolatedRunRequest) (isolate.IsolatedRunResult, error) {
	mode := strings.TrimPrefix(filepath.Base(request.Executable), "renderer-")
	switch mode {
	case "failure":
		return isolatedResult(runnerIdentity, request, nil), errors.Join(isolate.ErrChildFailed, errors.New("private-stderr-token"))
	case "wait":
		<-ctx.Done()
		return isolatedResult(runnerIdentity, request, nil), ctx.Err()
	case "unbounded-output":
		return isolatedResult(runnerIdentity, request, nil), isolate.ErrChildOutputTooLarge
	case "oversized":
		return isolatedResult(runnerIdentity, request, []byte(strings.Repeat("x", 2048))), nil
	case "malformed":
		return isolatedResult(runnerIdentity, request, []byte("{")), nil
	}
	digest := sha256.Sum256(request.Stdin)
	type outputPage struct {
		Number      int    `json:"number"`
		Text        string `json:"text"`
		EmptyReason string `json:"empty_reason,omitempty"`
	}
	response := struct {
		ContractVersion string       `json:"contract_version"`
		RuntimeIdentity string       `json:"runtime_identity"`
		SourceSHA256    string       `json:"source_sha256"`
		SourceBytes     int64        `json:"source_bytes"`
		Complete        bool         `json:"complete"`
		PageCount       int          `json:"page_count"`
		Pages           []outputPage `json:"pages"`
	}{
		ContractVersion: protocolVersion, RuntimeIdentity: testRuntimeIdentity,
		SourceSHA256: hex.EncodeToString(digest[:]), SourceBytes: int64(len(request.Stdin)),
		Complete: true, PageCount: 2,
	}
	response.Pages = append(response.Pages,
		outputPage{Number: 1, Text: "first page"},
		outputPage{Number: 2, Text: "second page"},
	)
	if mode == "many-pages" {
		const pages = 20_000
		response.PageCount = pages
		response.Pages = make([]outputPage, pages)
		for index := range pages {
			response.Pages[index] = outputPage{Number: index + 1, Text: "synthetic page text"}
		}
	}
	switch mode {
	case "version-drift":
		response.ContractVersion = "docbank-pymupdf/v2"
	case "runtime-drift":
		response.RuntimeIdentity = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	case "source-hash-drift":
		response.SourceSHA256 = strings.Repeat("b", 64)
	case "source-size-drift":
		response.SourceBytes++
	case "partial":
		response.Complete = false
	case "page-count-drift":
		response.PageCount++
	case "gap":
		response.Pages[1].Number = 3
	case "duplicate":
		response.Pages[1].Number = 1
	case "empty-unexplained":
		response.Pages[0].Text = ""
	case "empty-explained":
		response.Pages[0].Text = ""
		response.Pages[0].EmptyReason = "blank page"
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return isolate.IsolatedRunResult{}, err
	}
	if mode == "unknown-field" {
		encoded = append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	}
	return isolatedResult(runnerIdentity, request, encoded), nil
}

func isolatedResult(runnerIdentity string, request isolate.IsolatedRunRequest, stdout []byte) isolate.IsolatedRunResult {
	stdinDigest := sha256.Sum256(request.Stdin)
	return isolate.IsolatedRunResult{Stdout: stdout, Attestation: isolate.IsolationAttestation{
		RunnerIdentity: runnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
		ExecutableSHA256: request.ExecutableSHA256, StdinSHA256: hex.EncodeToString(stdinDigest[:]),
		NetworkDisabled: true, ProcessTreeContained: true, DigestVerifiedLaunch: true,
	}}
}

func TestExecutableDigestRejectsEmptyMalformedAndWrongContent(t *testing.T) {
	profile := testProfile(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	for _, digest := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("A", 64), strings.Repeat("0", 64)} {
		t.Run(digest, func(t *testing.T) {
			changed := profile
			changed.ExecutableSHA256 = digest
			_, err := New(changed)
			require.ErrorContains(t, err, "executable SHA-256")
		})
	}
}

func TestNativeIdentityRotationChangesProviderDescriptor(t *testing.T) {
	profile := testProfile(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	profile.Runner = &recordingRunner{identity: "sha256:4fb9848ec9197e12848559df3002bd39ceaa0377ae994bc8fbe9c8f082288d9e"}
	old, err := New(profile)
	require.NoError(t, err)
	profile.Runner = &recordingRunner{identity: "sha256:024e545bfc708239377cad1d6ec82674b536252c6206af46fbeb63fe376ead1b"}
	current, err := New(profile)
	require.NoError(t, err)
	assert.NotEqual(t, old.Descriptor().PolicyFingerprint, current.Descriptor().PolicyFingerprint)
	assert.NotEqual(t, old.Descriptor().Fingerprint, current.Descriptor().Fingerprint)
}

func TestProfileV2BindsImmutableExecutionAuthority(t *testing.T) {
	profile := testProfile(t, helperExecutable(t, "success"), time.Second, 1024)
	provider, err := New(profile)
	require.NoError(t, err)
	assert.Equal(t, "docbank-pymupdf-profile/v2", profileVersion)
	for _, change := range []func(*Profile){
		func(p *Profile) { p.Runner = &recordingRunner{identity: testRuntimeIdentity} },
		func(p *Profile) { p.RuntimeIdentity += ".revision" },
		func(p *Profile) { p.Executable = helperExecutable(t, "other") },
	} {
		changed := profile
		change(&changed)
		other, err := New(changed)
		require.NoError(t, err)
		assert.NotEqual(t, provider.Descriptor().PolicyFingerprint, other.Descriptor().PolicyFingerprint)
	}
	require.NoError(t, os.WriteFile(profile.Executable, []byte("synthetic replacement"), 0o700))
	profile.ExecutableSHA256, err = isolate.HashExecutable(t.Context(), profile.Executable)
	require.NoError(t, err)
	other, err := New(profile)
	require.NoError(t, err)
	assert.NotEqual(t, provider.Descriptor().PolicyFingerprint, other.Descriptor().PolicyFingerprint)
}
