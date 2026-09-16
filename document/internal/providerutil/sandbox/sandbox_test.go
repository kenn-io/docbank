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

func TestPolicyValidateRejectsUnsafeWorkNames(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	base := Policy{
		Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 1, MaxStdoutBytes: 1, WorkBytes: 1,
	}
	for _, name := range []string{"", "-input", "../input", "input/output", "input\\output", "input:name", "input\x00name"} {
		policy := base
		policy.Supervision = Supervision{Mode: SupervisedFileMode, InputName: name, OutputName: "output", WorkBytes: 1, MaxOutputBytes: 1}
		require.Error(t, policy.Validate(), name)
	}
}

func TestControlRoundTripIsBoundedAndAuthenticated(t *testing.T) {
	executable, err := filepath.Abs("renderer")
	require.NoError(t, err)
	policy := Policy{
		Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
		Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
		Directory: filepath.Dir(executable), MaxStdinBytes: 10, MaxStdoutBytes: 10, WorkBytes: 1,
	}
	control := launchControl{Policy: policy, PolicyFingerprint: strings.Repeat("b", sha256.Size*2), StdinSHA256: strings.Repeat("c", sha256.Size*2)}
	encoded, err := encodeControl(control)
	require.NoError(t, err)
	decoded, err := decodeControl(encoded)
	require.NoError(t, err)
	assert.Equal(t, control.Policy.Executable, decoded.Policy.Executable)
	assert.Equal(t, control.PolicyFingerprint, decoded.PolicyFingerprint)
	assert.Equal(t, control.StdinSHA256, decoded.StdinSHA256)
	_, err = decodeControl([]byte(strings.Repeat("x", int(MaxControlBytes)+1)))
	require.Error(t, err)
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
			Executable: executable, ExecutableSHA256: strings.Repeat("a", sha256.Size*2),
			Arguments: []string{"--fixed"}, Environment: []string{"LANG=C"},
			Directory: filepath.Dir(executable), MaxStdinBytes: 3, MaxStdoutBytes: 3, WorkBytes: 1,
		},
		Stdin: []byte("abc"), StdinSHA256: hex.EncodeToString(make([]byte, sha256.Size)),
	}
	require.Error(t, request.validate())
}
