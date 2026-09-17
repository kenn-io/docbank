package renderpdf

import (
	"context"
	"errors"
	"path/filepath"

	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

// Runner executes each stage with a verified executable and runtime snapshot.
// Injected runners must enforce and attest the same isolation and byte identities
// as the native runner, including hashing every runtime file before launch.
// Run may mutate request slices and retains ownership of its returned output.
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
	WarmupInput       []byte
	WarmupInputSHA256 string
	MaxOutputBytes    int64
	MaxWorkBytes      int64
	PolicyFingerprint string
	Runtime           []RuntimeFile
	RuntimeSymlinks   []RuntimeSymlink
	RuntimeIdentity   string
}

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
	Attestation Attestation
}

type nativeRunner struct{}

func (runner nativeRunner) Identity() string { return sandbox.NativeRunnerIdentity }

func (runner nativeRunner) Run(ctx context.Context, request Request) (StageResult, error) {
	if err := validateRequest(request); err != nil {
		return StageResult{}, err
	}
	policy := sandbox.Policy{
		Mode:       sandbox.LibreOfficeMode,
		Executable: request.Executable, ExecutableSHA256: request.ExecutableSHA256,
		Arguments: request.Arguments, Environment: request.Environment,
		Directory: request.Directory, MaxStdinBytes: max(int64(len(request.Input)), 1),
		MaxStdoutBytes: request.MaxOutputBytes,
		PrivateRoot: &sandbox.PrivateRoot{
			Runtime: request.Runtime, Symlinks: request.RuntimeSymlinks,
			RuntimeIdentity: request.RuntimeIdentity, WorkBytes: request.MaxWorkBytes,
			InputName: request.InputName, OutputName: request.OutputName,
			MaxOutputBytes: request.MaxOutputBytes,
			WarmupInput:    request.WarmupInput, WarmupInputSHA256: request.WarmupInputSHA256,
		},
	}
	sandboxResult, err := sandbox.Run(ctx, sandbox.Request{
		PolicyFingerprint: request.PolicyFingerprint, Policy: policy,
		Stdin: request.Input, StdinSHA256: request.InputSHA256,
	})
	result := StageResult{
		Output: sandboxResult.Stdout,
		Attestation: Attestation{
			RunnerIdentity: runner.Identity(), PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.ExecutableSHA256, InputSHA256: request.InputSHA256,
			OutputSHA256:         digest(sandboxResult.Stdout),
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
	return result, err
}

func validateRequest(request Request) error {
	if request.Stage == "" || request.Executable == "" || request.ExecutableSHA256 == "" ||
		request.InputName == "" || request.OutputName == "" || request.MaxOutputBytes <= 0 ||
		request.MaxWorkBytes <= 0 || len(request.Input) == 0 || request.RuntimeIdentity == "" ||
		len(request.WarmupInput) == 0 || request.WarmupInputSHA256 == "" {
		return errors.New("render PDF stage request is incomplete")
	}
	if filepath.Base(request.InputName) != request.InputName || filepath.Base(request.OutputName) != request.OutputName {
		return errors.New("render PDF stage names are invalid")
	}
	return nil
}
