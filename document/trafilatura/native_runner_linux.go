//go:build linux

package trafilatura

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"

	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

const nativeRunnerIdentity = sandbox.NativeRunnerIdentity

type nativeRunner struct {
	runner sandbox.Runner
}

func validateNativeExecutable(path string) error {
	executable, err := elf.Open(path)
	if err != nil {
		return errors.New("trafilatura: native runner requires an ELF executable")
	}
	_ = executable.Close()
	return nil
}

func newNativeRunner() (IsolatedRunner, error) {
	runner, err := sandbox.NewNativeRunner()
	if err != nil {
		return nil, err
	}
	return nativeRunner{runner: runner}, nil
}

func (runner nativeRunner) Identity() string { return nativeRunnerIdentity }

func (runner nativeRunner) Run(
	ctx context.Context, request IsolatedRunRequest,
) (IsolatedRunResult, error) {
	if err := validateNativeRequest(request); err != nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return IsolatedRunResult{}, errors.Join(errNativeCanceledBeforeLaunch, err)
	}
	if runner.runner == nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	sandboxRequest := sandbox.Request{
		PolicyFingerprint: request.PolicyFingerprint,
		Policy: sandbox.Policy{
			Mode:       sandbox.ExecMode,
			Executable: request.Executable, ExecutableSHA256: request.ExecutableSHA256,
			Arguments: slices.Clone(request.Arguments), Environment: slices.Clone(request.Environment),
			Directory: request.Directory, MaxStdinBytes: max(int64(len(request.Stdin)), 1),
			MaxStdoutBytes: request.MaxStdoutBytes,
		},
		Stdin: slices.Clone(request.Stdin), StdinSHA256: request.StdinSHA256,
	}
	result, runErr := runner.runner.Run(ctx, sandboxRequest)
	converted := IsolatedRunResult{
		Stdout: result.Stdout,
		Attestation: IsolationAttestation{
			RunnerIdentity: nativeRunnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.ExecutableSHA256, StdinSHA256: request.StdinSHA256,
			NetworkDisabled:      result.Attestation.NetworkDisabled,
			ProcessTreeContained: result.Attestation.ProcessTreeContained,
			DigestVerifiedLaunch: result.Attestation.DigestVerifiedLaunch,
			FilesystemIsolated:   result.Attestation.FilesystemIsolated,
		},
	}
	if errors.Is(runErr, sandbox.ErrCanceledBeforeLaunch) && ctx.Err() != nil {
		return converted, errors.Join(errNativeCanceledBeforeLaunch, ctx.Err())
	}
	switch {
	case errors.Is(runErr, sandbox.ErrUnavailable):
		return converted, ErrIsolationUnavailable
	case errors.Is(runErr, sandbox.ErrOutputTooLarge):
		return converted, ErrChildOutputTooLarge
	case errors.Is(runErr, sandbox.ErrChildFailed):
		return converted, ErrChildFailed
	default:
		return converted, runErr
	}
}

func validateNativeRequest(request IsolatedRunRequest) error {
	stdinDigest := sha256.Sum256(request.Stdin)
	if !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable ||
		request.Directory != filepath.Dir(request.Executable) ||
		!slices.Equal(request.Arguments, []string{"--protocol", protocolVersion}) ||
		!slices.Equal(request.Environment, cleanEnvironment()) ||
		request.StdinSHA256 != hex.EncodeToString(stdinDigest[:]) ||
		request.MaxStdoutBytes <= 0 || request.MaxStdoutBytes > MaxResponseBytes ||
		!request.Requirements.NetworkDisabled || !request.Requirements.KillProcessTree ||
		!request.Requirements.VerifyExecutableSHA256 || !request.Requirements.FilesystemIsolated ||
		request.PolicyFingerprint != isolationRequestPolicyFingerprint(nativeRunnerIdentity, request) {
		return errors.New("native isolation request is outside the fixed policy")
	}
	if err := validateSHA256(request.ExecutableSHA256, "executable SHA-256"); err != nil {
		return err
	}
	return nil
}

var _ IsolatedRunner = nativeRunner{}
