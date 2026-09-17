package renderpdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"

	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

type Runner interface {
	Identity() string
	Run(ctx context.Context, request Request) (StageResult, error)
}

type Request struct {
	Stage             string
	Executable        string
	ExecutableSHA256  string
	Arguments         []string
	Environment       []string
	Directory         string
	InputName         string
	OutputName        string
	Input             []byte
	InputSHA256       string
	MaxOutputBytes    int64
	MaxWorkBytes      int64
	PolicyFingerprint string
	Runtime           []RuntimeFile
	RuntimeSymlinks   []RuntimeSymlink
	RuntimeIdentity   string
}

type RunRequest = Request

type Attestation struct {
	RunnerIdentity       string
	PolicyFingerprint    string
	ExecutableSHA256     string
	InputSHA256          string
	OutputSHA256         string
	NetworkDisabled      bool
	ProcessTreeContained bool
	DigestVerifiedLaunch bool
	FilesystemIsolated   bool
	FilesystemMode       string
	PrivateRootInstalled bool
	RuntimeIdentity      string
	UnixIPCAllowed       bool
	RestartCount         int
}

type StageResult struct {
	Output      []byte
	Stdout      []byte
	Attestation Attestation
}

type RunnerResult = StageResult
type RunResult = StageResult

type nativeRunner struct {
	runtime         []RuntimeFile
	runtimeSymlinks []RuntimeSymlink
	runtimeIdentity string
}

func newNativeRunner(renderer Renderer) (Runner, error) {
	if len(renderer.Runtime) == 0 || renderer.RuntimeIdentity == "" {
		return nil, sandbox.ErrRuntimeIdentityMismatch
	}
	_, err := sandbox.NewNativeRunner()
	if err != nil {
		return nil, err
	}
	return nativeRunner{
		runtime: slices.Clone(renderer.Runtime), runtimeSymlinks: slices.Clone(renderer.RuntimeSymlinks),
		runtimeIdentity: renderer.RuntimeIdentity,
	}, nil
}

func (runner nativeRunner) Identity() string { return sandbox.NativeRunnerIdentity }

func (runner nativeRunner) Run(ctx context.Context, request Request) (StageResult, error) {
	if err := validateRequest(request); err != nil {
		return StageResult{}, sandbox.ErrUnavailable
	}
	policy := sandbox.Policy{
		Mode:       sandbox.SupervisedFileMode,
		Executable: request.Executable, ExecutableSHA256: request.ExecutableSHA256,
		Arguments: slices.Clone(request.Arguments), Environment: slices.Clone(request.Environment),
		Directory: request.Directory, MaxStdinBytes: max(int64(len(request.Input)), 1),
		MaxStdoutBytes: request.MaxOutputBytes,
		PrivateRoot: &sandbox.PrivateRoot{
			Runtime: slices.Clone(request.Runtime), Symlinks: slices.Clone(request.RuntimeSymlinks),
			RuntimeIdentity: request.RuntimeIdentity, WorkBytes: request.MaxWorkBytes,
			InputName: request.InputName, OutputName: request.OutputName,
			MaxOutputBytes: request.MaxOutputBytes, UnixIPC: true,
		},
	}
	sandboxResult, err := sandbox.Run(ctx, sandbox.Request{
		PolicyFingerprint: request.PolicyFingerprint, Policy: policy,
		Stdin: slices.Clone(request.Input), StdinSHA256: request.InputSHA256,
	})
	result := StageResult{
		Output: slices.Clone(sandboxResult.Output), Stdout: slices.Clone(sandboxResult.Stdout),
		Attestation: Attestation{
			RunnerIdentity: runner.Identity(), PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.ExecutableSHA256, InputSHA256: request.InputSHA256,
			OutputSHA256:         digestBytes(sandboxResult.Output),
			NetworkDisabled:      sandboxResult.Attestation.NetworkDisabled,
			ProcessTreeContained: sandboxResult.Attestation.ProcessTreeContained,
			DigestVerifiedLaunch: sandboxResult.Attestation.DigestVerifiedLaunch,
			FilesystemIsolated:   sandboxResult.Attestation.FilesystemIsolated,
			FilesystemMode:       sandboxResult.Attestation.FilesystemMode,
			PrivateRootInstalled: sandboxResult.Attestation.PrivateRootInstalled,
			RuntimeIdentity:      sandboxResult.Attestation.RuntimeIdentity,
			UnixIPCAllowed:       sandboxResult.Attestation.UnixIPCAllowed,
			RestartCount:         sandboxResult.Attestation.RestartCount,
		},
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func validateRequest(request Request) error {
	if request.Stage == "" || request.Executable == "" || request.ExecutableSHA256 == "" ||
		request.InputName == "" || request.OutputName == "" || request.MaxOutputBytes <= 0 ||
		request.MaxWorkBytes <= 0 || len(request.Input) == 0 || request.RuntimeIdentity == "" {
		return errors.New("render PDF stage request is incomplete")
	}
	if filepath.Base(request.InputName) != request.InputName || filepath.Base(request.OutputName) != request.OutputName {
		return errors.New("render PDF stage names are invalid")
	}
	return nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
