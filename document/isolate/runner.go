package isolate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	// MaxExecutableBytes bounds executable identity verification.
	MaxExecutableBytes = int64(256 << 20)
	// MaxResponseBytes bounds native child output.
	MaxResponseBytes       = int64(64 << 20)
	nativeRunnerDescriptor = "docbank-isolate-native/v3:trafilatura-v2+pymupdf-v1,sealed-argv-env,user+net+pid+mount-namespaces,private-readonly-proc,no-new-privs,seccomp-network+io-uring+abi,sealed-executable,parent-death,output-bound,tree-cleanup"
	nativeRunnerIdentity   = "sha256:024e545bfc708239377cad1d6ec82674b536252c6206af46fbeb63fe376ead1b"
)

var (
	// ErrIsolationUnavailable means required native controls could not be enforced.
	ErrIsolationUnavailable = errors.New("isolated runner policy unavailable")
	// ErrChildOutputTooLarge means the child exceeded its stdout allowance.
	ErrChildOutputTooLarge = errors.New("isolated child output exceeds limit")
	// ErrChildFailed means the child exited without a valid response.
	ErrChildFailed = errors.New("isolated child failed")
	// ErrCanceledBeforeLaunch means cancellation prevented launch and no child needs cleanup.
	ErrCanceledBeforeLaunch = errors.New("native isolated runner canceled before launch")
)

// IsolationRequirements are mandatory runner controls. A runner must fail
// closed rather than execute when any requested control is unavailable.
type IsolationRequirements struct {
	NetworkDisabled        bool
	KillProcessTree        bool
	VerifyExecutableSHA256 bool
}

// IsolatedRunRequest is the complete, fixed child execution authority.
type IsolatedRunRequest struct {
	Executable        string
	ExecutableSHA256  string
	Arguments         []string
	Environment       []string
	Directory         string
	Stdin             []byte
	StdinSHA256       string
	MaxStdoutBytes    int64
	PolicyFingerprint string
	Requirements      IsolationRequirements
}

// IsolationAttestation reports the exact controls applied to a completed run.
type IsolationAttestation struct {
	RunnerIdentity       string
	PolicyFingerprint    string
	ExecutableSHA256     string
	StdinSHA256          string
	NetworkDisabled      bool
	ProcessTreeContained bool
	DigestVerifiedLaunch bool
}

// IsolatedRunResult is bounded stdout plus its isolation attestation.
type IsolatedRunResult struct {
	Stdout      []byte
	Attestation IsolationAttestation
}

// IsolatedRunner is the trusted cross-platform process isolation boundary.
// Run must launch the digest-verified executable without a path re-open race,
// deny all network access, contain the process tree, and reap that tree on
// cancellation. It must return ErrIsolationUnavailable rather than weaken a
// requested control.
type IsolatedRunner interface {
	Identity() string
	Run(ctx context.Context, request IsolatedRunRequest) (IsolatedRunResult, error)
}

// RequestPolicyFingerprint binds the exact request to the runner identity.
func RequestPolicyFingerprint(runnerIdentity string, request IsolatedRunRequest) string {
	identity := strings.Join([]string{
		"docbank-isolated-run/v1", runnerIdentity, request.Executable,
		request.ExecutableSHA256, strings.Join(request.Arguments, "\x1f"), strings.Join(request.Environment, "\x1f"),
		request.Directory, request.StdinSHA256, strconv.FormatInt(request.MaxStdoutBytes, 10),
		"network-disabled", "kill-process-tree", "digest-verified-launch",
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// HashExecutable verifies a bounded regular executable and hashes its complete bytes.
func HashExecutable(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return "", errors.New("executable identity is outside the supported bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(contextReader{ctx: ctx, reader: file}, MaxExecutableBytes+1))
	if err != nil || written <= 0 || written > MaxExecutableBytes {
		return "", errors.New("executable content could not be bounded and hashed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(value)
}

func validateNativeRequest(request IsolatedRunRequest) error {
	stdinDigest := sha256.Sum256(request.Stdin)
	if !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable || len(request.Executable) > 4096 || strings.ContainsRune(request.Executable, '\x00') ||
		request.Directory != filepath.Dir(request.Executable) ||
		!validNativeArguments(request.Arguments) ||
		!slices.Equal(request.Environment, nativeEnvironment()) ||
		request.StdinSHA256 != hex.EncodeToString(stdinDigest[:]) ||
		request.MaxStdoutBytes <= 0 || request.MaxStdoutBytes > MaxResponseBytes ||
		!request.Requirements.NetworkDisabled || !request.Requirements.KillProcessTree ||
		!request.Requirements.VerifyExecutableSHA256 ||
		request.PolicyFingerprint != RequestPolicyFingerprint(nativeRunnerIdentity, request) {
		return errors.New("native isolation request is outside the fixed policy")
	}
	decoded, err := hex.DecodeString(request.ExecutableSHA256)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != request.ExecutableSHA256 {
		return errors.New("invalid executable SHA-256")
	}
	return nil
}

func validNativeArguments(arguments []string) bool {
	return len(arguments) == 2 && arguments[0] == "--protocol" &&
		(arguments[1] == "docbank-trafilatura/v2" || arguments[1] == "docbank-pymupdf/v1")
}

func nativeEnvironment() []string {
	return []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "PYTHONHASHSEED=0", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1"}
}
