package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyValidateRejectsUnsafeRuntimeEntries(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	base := Policy{
		Mode: ExecMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 1, MaxStdoutBytes: 1,
	}
	root := &PrivateRoot{
		RuntimeIdentity: "sha256:" + strings.Repeat("b", sha256.Size*2),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
		Runtime: []RuntimeFile{{SourcePath: executable, GuestPath: "relative", SHA256: strings.Repeat("c", sha256.Size*2)}},
	}
	policy := base
	policy.Mode = SupervisedFileMode
	policy.PrivateRoot = root
	require.Error(t, policy.Validate())
	root.Runtime[0].GuestPath = "/usr/bin/renderer"
	root.Symlinks = []RuntimeSymlink{{GuestPath: "/usr/bin/link", Target: "../renderer"}}
	require.Error(t, policy.Validate())
}

func TestControlRoundTripIsBoundedAndAuthenticated(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	policy := Policy{
		Mode: ExecMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 10, MaxStdoutBytes: 10,
	}
	control := launchControl{Policy: policy, PolicyFingerprint: strings.Repeat("b", sha256.Size*2), StdinSHA256: strings.Repeat("c", sha256.Size*2)}
	encoded, err := encodeControl(control)
	require.NoError(t, err)
	decoded, err := decodeControl(encoded)
	require.NoError(t, err)
	assert.Equal(t, control.Policy.Executable, decoded.Policy.Executable)
	assert.Equal(t, control.PolicyFingerprint, decoded.PolicyFingerprint)
	assert.Equal(t, control.StdinSHA256, decoded.StdinSHA256)
	_, err = decodeControl([]byte(strings.Repeat("x", int(MaxExecControlBytes)+1)))
	require.Error(t, err)
}

func TestPrivateRootControlUsesEightMiBCeiling(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	root := &PrivateRoot{
		RuntimeIdentity: "sha256:" + strings.Repeat("b", sha256.Size*2),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
	}
	policy := Policy{
		Mode: SupervisedFileMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 1, MaxStdoutBytes: 1,
		PrivateRoot: root,
	}
	control := launchControl{Policy: policy, StdinSHA256: strings.Repeat("c", sha256.Size*2)}
	encoded, err := encodeControl(control)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), int(MaxPrivateRootControlBytes))
}

func TestPolicyValidateBindsWarmupBytesToDigestAndLimit(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	root := &PrivateRoot{
		RuntimeIdentity: "sha256:" + strings.Repeat("b", sha256.Size*2),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
	}
	policy := Policy{
		Mode: SupervisedFileMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 1, MaxStdoutBytes: 1,
		PrivateRoot: root,
	}
	root.WarmupInputSHA256 = strings.Repeat("0", sha256.Size*2)
	require.ErrorContains(t, policy.Validate(), "does not match content")
	root.WarmupInput = make([]byte, MaxWarmupBytes+1)
	root.WarmupInputSHA256 = hex.EncodeToString(make([]byte, sha256.Size))
	require.ErrorContains(t, policy.Validate(), "outside the supported bound")
}

func TestRunUnavailableOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	_, err := Run(context.Background(), Request{})
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestRequestRejectsMismatchedInputDigest(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	request := Request{
		Policy: Policy{
			Mode: ExecMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
			Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
			Directory: filepath.Dir(executable), MaxStdinBytes: 3, MaxStdoutBytes: 3,
		},
		Stdin: []byte("abc"), StdinSHA256: hex.EncodeToString(make([]byte, sha256.Size)),
	}
	require.Error(t, request.validate())
}
