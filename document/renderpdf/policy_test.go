package renderpdf

import (
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyFingerprintIncludesExecutedStageArguments(t *testing.T) {
	runner := &recordingRunner{identity: testRunnerIdentity, output: flatODF(FlatTextKind, "<text:p/>")}
	policy := testPolicy(t, runner)
	_, err := Convert(t.Context(), testSource(t, zipDocument(t, "docx"), docxMediaType), "docx", policy)
	require.NoError(t, err)
	encoded, err := encodePolicyFingerprint(policy.renderer, policy.runnerID, policy.limits, warmupFixtureDigests())
	require.NoError(t, err)
	var fingerprint struct {
		Arguments  map[string][]string `json:"arguments"`
		WarmupKeys map[string]string   `json:"warmup_keys"`
	}
	require.NoError(t, json.Unmarshal(encoded, &fingerprint))
	profile, ok := profileFor("docx")
	require.True(t, ok)
	for _, request := range runner.calls {
		assert.Equal(t, request.Arguments, fingerprint.Arguments[profileStageKey(profile, request.Stage)])
	}
	assert.Len(t, fingerprint.Arguments, len(renderProfiles)*2)
	for _, profile := range renderProfiles {
		for _, stage := range []string{"normalize", "pdf"} {
			key := profileStageKey(profile, stage)
			request := stageRequest(policy, profile, stage, []byte("synthetic input"))
			assert.Equal(t, request.Arguments, fingerprint.Arguments[key])
			assert.Equal(t, warmupKey(profile, stage), fingerprint.WarmupKeys[key])
			assert.Equal(t, digest(request.WarmupInput), request.WarmupInputSHA256)
		}
	}
}

func TestNewPolicyValidatesIdentityAndTighteningLimits(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	runner := &recordingRunner{identity: testRunnerIdentity}
	runtimeFile := RuntimeFile{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true}
	runtimeIdentity, err := runtimeIdentityForManifest([]RuntimeFile{runtimeFile}, nil)
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.MaxSourceBytes = 1 << 20
	policy, err := NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: digest(content), Runtime: []RuntimeFile{runtimeFile}, RuntimeIdentity: runtimeIdentity,
		Runner: runner,
	}, limits)
	require.NoError(t, err)
	assert.Len(t, policy.Fingerprint(), 64)
	assert.Equal(t, limits, policy.Limits())
	assert.Equal(t, testRunnerIdentity, policy.RunnerIdentity())
}

func TestPolicyFingerprintIncludesWarmupDigest(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	runtimeFile := RuntimeFile{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true}
	runtimeIdentity, err := runtimeIdentityForManifest([]RuntimeFile{runtimeFile}, nil)
	require.NoError(t, err)
	renderer := Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runtime: []RuntimeFile{runtimeFile}, RuntimeIdentity: runtimeIdentity,
	}
	warmups := warmupFixtureDigests()
	first, err := encodePolicyFingerprint(renderer, testRunnerIdentity, DefaultLimits(), warmups)
	require.NoError(t, err)
	for index := range warmups {
		mutated := append([]string(nil), warmups...)
		mutated[index] = strings.Repeat("0", 64)
		second, err := encodePolicyFingerprint(renderer, testRunnerIdentity, DefaultLimits(), mutated)
		require.NoError(t, err)
		assert.NotEqual(t, digest(first), digest(second), "warm-up digest %d", index)
	}
}

func TestPolicyFingerprintIncludesWarmupKeys(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	runtimeFile := RuntimeFile{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true}
	runtimeIdentity, err := runtimeIdentityForManifest([]RuntimeFile{runtimeFile}, nil)
	require.NoError(t, err)
	renderer := Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runtime: []RuntimeFile{runtimeFile}, RuntimeIdentity: runtimeIdentity,
	}
	first, err := encodePolicyFingerprint(renderer, testRunnerIdentity, DefaultLimits(), warmupFixtureDigests())
	require.NoError(t, err)
	original := renderProfiles[1].sourceWarmup
	defer func() { renderProfiles[1].sourceWarmup = original }()
	renderProfiles[1].sourceWarmup = "docx"
	second, err := encodePolicyFingerprint(renderer, testRunnerIdentity, DefaultLimits(), warmupFixtureDigests())
	require.NoError(t, err)
	assert.NotEqual(t, digest(first), digest(second))
}

func TestNewPolicyEnforcesSelectedRuntimeEntryLimit(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	runtimeFiles := []RuntimeFile{
		{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true},
		{SourcePath: executable, GuestPath: "/usr/lib/test-runtime", SHA256: digest(content)},
	}
	runtimeIdentity, err := runtimeIdentityForManifest(runtimeFiles, nil)
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.MaxRuntimeEntries = 1
	_, err = NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runtime: runtimeFiles, RuntimeIdentity: runtimeIdentity,
		Runner: &recordingRunner{identity: testRunnerIdentity},
	}, limits)
	require.ErrorContains(t, err, "configured entry limit")
}

func TestNewPolicyRejectsInvalidIdentitiesAndLimits(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	base := Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		Runner: &recordingRunner{identity: testRunnerIdentity},
	}
	runtimeFile := RuntimeFile{SourcePath: executable, GuestPath: "/usr/bin/test-runner", SHA256: digest(content), Executable: true}
	runtimeIdentity, err := runtimeIdentityForManifest([]RuntimeFile{runtimeFile}, nil)
	require.NoError(t, err)
	base.Runtime = []RuntimeFile{runtimeFile}
	base.RuntimeIdentity = runtimeIdentity
	for _, testCase := range []struct {
		name string
		edit func(*Renderer, *Limits)
	}{
		{name: "runtime identity", edit: func(renderer *Renderer, _ *Limits) { renderer.RuntimeIdentity = "mutable" }},
		{name: "runner identity", edit: func(renderer *Renderer, _ *Limits) { renderer.Runner = &recordingRunner{identity: "mutable"} }},
		{name: "source limit", edit: func(_ *Renderer, limits *Limits) { limits.MaxSourceBytes = 0 }},
		{name: "limit above ceiling", edit: func(_ *Renderer, limits *Limits) { limits.MaxPDFBytes = DefaultLimits().MaxPDFBytes + 1 }},
		{name: "executable path", edit: func(renderer *Renderer, _ *Limits) {
			renderer.Executable = strings.ReplaceAll(renderer.Executable, string(os.PathSeparator), "//")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			renderer := base
			limits := DefaultLimits()
			testCase.edit(&renderer, &limits)
			_, err := NewPolicy(renderer, limits)
			require.Error(t, err)
		})
	}
}
