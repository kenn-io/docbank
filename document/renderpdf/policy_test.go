package renderpdf

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
