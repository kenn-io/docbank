package trafilatura

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
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
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 10, Timeout: time.Second,
	}

	_, err = New(profile)
	require.NoError(t, err)

	profile.Runner = nil
	if runtime.GOOS == "linux" {
		profile.Executable, err = os.Executable()
		require.NoError(t, err)
		executableBytes, err = os.ReadFile(profile.Executable)
		require.NoError(t, err)
		digest = sha256.Sum256(executableBytes)
		profile.ExecutableSHA256 = hex.EncodeToString(digest[:])
	}
	provider, err := New(profile)
	if runtime.GOOS == "linux" {
		require.NoError(t, err)
		require.NotNil(t, provider.runner)
	} else {
		require.ErrorIs(t, err, ErrIsolationUnavailable)
	}

	profile.Runner = &recordingRunner{identity: "mutable-latest"}
	_, err = New(profile)
	require.ErrorContains(t, err, "runner identity")

	profile.Runner = &recordingRunner{identity: testRunnerIdentity}
	profile.ExecutableSHA256 = ""
	_, err = New(profile)
	require.ErrorContains(t, err, "executable SHA-256")

	profile.ExecutableSHA256 = hex.EncodeToString(digest[:])
	profile.RuntimeIdentity = "mutable-latest"
	_, err = New(profile)
	require.ErrorContains(t, err, "runtime identity")
}

func TestProviderDelegatesOnlyAnExactFailClosedIsolationRequest(t *testing.T) {
	t.Setenv("DOCBANK_TRAFILATURA_AMBIENT_SECRET", "must-not-reach-runner")
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	source := []byte(`<!doctype html><html><body><h1>Local title</h1><p>Local body</p><a href="https://private.example/a">link</a></body></html>`)
	upload := newTestUpload(source, "article.html", "text/html")

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
	if runtime.GOOS == "windows" {
		if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
			expectedEnvironment = append(expectedEnvironment, "SystemRoot="+systemRoot)
		}
	}
	assert.Equal(t, expectedEnvironment, runner.request.Environment)
	assert.NotContains(t, strings.Join(runner.request.Environment, "\n"), "must-not-reach-runner")
	assert.Equal(t, filepath.Dir(executable), runner.request.Directory)
	assert.Equal(t, source, runner.request.Stdin)
	stdinDigest := sha256.Sum256(source)
	assert.Equal(t, hex.EncodeToString(stdinDigest[:]), runner.request.StdinSHA256)
	assert.Equal(t, int64(1<<20), runner.request.MaxStdoutBytes)
	assert.Equal(t, IsolationRequirements{
		NetworkDisabled: true, KillProcessTree: true, VerifyExecutableSHA256: true,
		FilesystemIsolated: true,
	}, runner.request.Requirements)
	require.Len(t, runner.request.PolicyFingerprint, 64)
}

func TestProviderRejectsIncompleteOrMismatchedIsolationAttestation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*IsolationAttestation)
	}{
		{name: "runner identity", mutate: func(value *IsolationAttestation) { value.RunnerIdentity = testRuntimeIdentity }},
		{name: "policy", mutate: func(value *IsolationAttestation) { value.PolicyFingerprint = strings.Repeat("0", 64) }},
		{name: "executable", mutate: func(value *IsolationAttestation) { value.ExecutableSHA256 = strings.Repeat("0", 64) }},
		{name: "stdin", mutate: func(value *IsolationAttestation) { value.StdinSHA256 = strings.Repeat("0", 64) }},
		{name: "network", mutate: func(value *IsolationAttestation) { value.NetworkDisabled = false }},
		{name: "process tree", mutate: func(value *IsolationAttestation) { value.ProcessTreeContained = false }},
		{name: "digest launch", mutate: func(value *IsolationAttestation) { value.DigestVerifiedLaunch = false }},
		{name: "filesystem", mutate: func(value *IsolationAttestation) { value.FilesystemIsolated = false }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			executable := helperExecutable(t, "complete")
			runner := &recordingRunner{identity: testRunnerIdentity}
			runner.run = func(ctx context.Context, request IsolatedRunRequest) (IsolatedRunResult, error) {
				result, err := defaultRun(ctx, runner.identity, request)
				testCase.mutate(&result.Attestation)
				return result, err
			}
			profile := testProfile(t, executable, time.Second, 1<<20)
			profile.Runner = runner
			provider, err := New(profile)
			require.NoError(t, err)
			upload := newTestUpload(testHTML, "article.html", "text/html")

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
			upload := newTestUpload(testHTML, "article.html", "text/html")

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
		context.Context, IsolatedRunRequest,
	) (IsolatedRunResult, error) {
		return IsolatedRunResult{}, errors.Join(ErrIsolationUnavailable, errors.New("private-runner-detail"))
	}}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testHTML, "article.html", "text/html")

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	assert.NotContains(t, err.Error(), "private-runner-detail")
}

func TestProviderRequiresProcessTreeCleanupAttestationAfterRunnerCancellation(t *testing.T) {
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity}
	runner.run = func(ctx context.Context, request IsolatedRunRequest) (IsolatedRunResult, error) {
		<-ctx.Done()
		result := isolatedResult(runner.identity, request, nil)
		result.Attestation.ProcessTreeContained = false
		return result, ctx.Err()
	}
	profile := testProfile(t, executable, 10*time.Millisecond, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testHTML, "article.html", "text/html")

	_, err = provider.Render(t.Context(), upload,
		testAuthorization(provider.Descriptor(), upload.Metadata()))
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
}

func TestProviderConstrainsRunnerStdoutToAuthorizedTotalResultBytes(t *testing.T) {
	executable := helperExecutable(t, "complete")
	runner := &recordingRunner{identity: testRunnerIdentity, run: func(
		context.Context, IsolatedRunRequest,
	) (IsolatedRunResult, error) {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}}
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = runner
	provider, err := New(profile)
	require.NoError(t, err)
	upload := newTestUpload(testHTML, "article.html", "text/html")
	authorization := testAuthorization(provider.Descriptor(), upload.Metadata())
	authorization.MaxTotalResultBytes = 128

	_, err = provider.Render(t.Context(), upload, authorization)
	assertProviderCode(t, err, document.RenditionErrorPolicyRejected)
	require.NotNil(t, runner.request)
	assert.Equal(t, int64(128), runner.request.MaxStdoutBytes)
}

type recordingRunner struct {
	identity string
	request  *IsolatedRunRequest
	run      func(context.Context, IsolatedRunRequest) (IsolatedRunResult, error)
}

func (runner *recordingRunner) Identity() string { return runner.identity }

func (runner *recordingRunner) Run(ctx context.Context, request IsolatedRunRequest) (IsolatedRunResult, error) {
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

func defaultRun(ctx context.Context, runnerIdentity string, request IsolatedRunRequest) (IsolatedRunResult, error) {
	mode := strings.TrimSuffix(filepath.Base(request.Executable), filepath.Ext(request.Executable))
	mode = strings.TrimPrefix(mode, "renderer-")
	switch mode {
	case "failure":
		return isolatedResult(runnerIdentity, request, nil),
			errors.Join(ErrChildFailed, errors.New("private-stderr-token"))
	case "wait":
		<-ctx.Done()
		return isolatedResult(runnerIdentity, request, nil), ctx.Err()
	case "slow-complete":
		select {
		case <-ctx.Done():
			return isolatedResult(runnerIdentity, request, nil), ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	case "unbounded-output":
		return isolatedResult(runnerIdentity, request, nil), ErrChildOutputTooLarge
	case "malformed":
		return isolatedResult(runnerIdentity, request, []byte("{")), nil
	}
	digest := sha256.Sum256(request.Stdin)
	value := response{ContractVersion: protocolVersion, RuntimeIdentity: testRuntimeIdentity,
		SourceSHA256: hex.EncodeToString(digest[:]), SourceBytes: int64(len(request.Stdin)),
		ExtractionComplete: true, ProvenanceComplete: new(false),
		Units: []responseUnit{{Text: "Local title Local body"}},
	}
	if strings.Contains(string(request.Stdin), "link") {
		value.Units[0].Text += " link"
	}
	switch mode {
	case "degraded":
		value.Units[0].Text = "Local title\n\nLocal body"
	case "fetched":
		value.Units[0].Text += " REMOTE_FETCH_TOKEN"
	case "version-drift":
		value.ContractVersion += ".next"
	case "runtime-drift":
		value.RuntimeIdentity = "sha256:" + strings.Repeat("c", 64)
	case "source-hash-drift":
		value.SourceSHA256 = strings.Repeat("c", 64)
	case "source-size-drift":
		value.SourceBytes++
	case "partial":
		value.Units = append(value.Units, responseUnit{})
	case "empty":
		value.Units = nil
	case "bad-heading":
		value.Units[0].Heading = "Fetched heading"
	case "complete-inexact":
		value.Units[0].Text = "Local title"
	case "extraction-incomplete":
		value.ExtractionComplete = false
	case "truncated-degraded":
		value.Units[0].Text = "Local title"
	case "structured":
		value.ProvenanceComplete = new(true)
		value.Units[0].SourcePath = "/html[1]/body[1]/section[1]"
		value.Units[0].Heading = "Local title"
	case "structured-path-drift":
		value.ProvenanceComplete = new(true)
		value.Units[0].SourcePath = "/html[1]/body[1]/section[2]"
		value.Units[0].Heading = "Local title"
	case "structured-boundary-drift":
		value.ProvenanceComplete = new(true)
		value.Units[0].SourcePath = "/html[1]/body[1]/section[1]"
		value.Units[0].Heading = "Local title"
		value.Units[0].Text = "Local title"
	case "structured-duplicate":
		value.ProvenanceComplete = new(true)
		value.Units = []responseUnit{
			{SourcePath: "/html[1]/body[1]/section[1]", Heading: "Repeat", Text: "Repeat First"},
			{SourcePath: "/html[1]/body[1]/section[2]", Heading: "Repeat", Text: "Repeat Second"},
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return IsolatedRunResult{}, err
	}
	if mode == "unknown-field" {
		encoded = append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	}
	if mode == "missing-provenance-complete" {
		encoded = bytes.Replace(encoded, []byte(`,"provenance_complete":false`), nil, 1)
	}
	return isolatedResult(runnerIdentity, request, encoded), nil
}

func isolatedResult(runnerIdentity string, request IsolatedRunRequest, stdout []byte) IsolatedRunResult {
	stdinDigest := sha256.Sum256(request.Stdin)
	return IsolatedRunResult{Stdout: stdout, Attestation: IsolationAttestation{
		RunnerIdentity: runnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
		ExecutableSHA256: request.ExecutableSHA256, StdinSHA256: hex.EncodeToString(stdinDigest[:]),
		NetworkDisabled: true, ProcessTreeContained: true, DigestVerifiedLaunch: true,
		FilesystemIsolated: true,
	}}
}
