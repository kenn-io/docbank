package renderpdf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	ConverterVersion      = "renderpdf-v1-libreoffice-flat-odf"
	WarmupContractVersion = "renderpdf-warmup-v1"
	MaxExecutableBytes    = sandbox.MaxExecutableBytes
)

type Renderer struct {
	Executable       string
	ExecutableSHA256 string
	Runtime          []RuntimeFile
	RuntimeSymlinks  []RuntimeSymlink
	RuntimeIdentity  string
	Runner           Runner
}

type Limits struct {
	MaxSourceBytes     int64         `json:"max_source_bytes"`
	MaxNormalizedBytes int64         `json:"max_normalized_bytes"`
	MaxPDFBytes        int64         `json:"max_pdf_bytes"`
	MaxWorkBytes       int64         `json:"max_work_bytes"`
	MaxXMLElements     int           `json:"max_xml_elements"`
	MaxXMLDepth        int           `json:"max_xml_depth"`
	MaxPages           int           `json:"max_pages"`
	MaxRuntimeEntries  int           `json:"max_runtime_entries"`
	Timeout            time.Duration `json:"timeout_ns"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxSourceBytes: 50 << 20, MaxNormalizedBytes: 200 << 20,
		MaxPDFBytes: 50 << 20, MaxWorkBytes: 512 << 20,
		MaxXMLElements: 2_000_000, MaxXMLDepth: 256, MaxPages: 1_000,
		MaxRuntimeEntries: sandbox.MaxRuntimeEntries, Timeout: 600 * time.Second,
	}
}

type Policy struct {
	renderer    Renderer
	limits      Limits
	runner      Runner
	runnerID    string
	fingerprint string
}

func NewPolicy(renderer Renderer, limits Limits) (Policy, error) {
	if !filepath.IsAbs(renderer.Executable) || filepath.Clean(renderer.Executable) != renderer.Executable {
		return Policy{}, errors.New("render PDF executable must be an absolute clean path")
	}
	if _, err := providerutil.LoadPinnedExecutable(renderer.Executable, renderer.ExecutableSHA256, MaxExecutableBytes); err != nil {
		return Policy{}, errors.New("render PDF executable is not pinned")
	}
	if err := validateImmutableIdentity(renderer.RuntimeIdentity, "renderer runtime identity"); err != nil {
		return Policy{}, err
	}
	if err := validateLimits(limits); err != nil {
		return Policy{}, err
	}
	if err := validateRuntimeEntries(renderer, limits.MaxRuntimeEntries); err != nil {
		return Policy{}, err
	}
	runner := renderer.Runner
	if runner == nil {
		if runtime.GOOS != "linux" {
			return Policy{}, sandbox.ErrUnavailable
		}
		native, err := newNativeRunner(renderer)
		if err != nil {
			return Policy{}, err
		}
		runner = native
	}
	if err := validateImmutableIdentity(runner.Identity(), "runner identity"); err != nil {
		return Policy{}, err
	}
	renderer.Runtime = slices.Clone(renderer.Runtime)
	renderer.RuntimeSymlinks = slices.Clone(renderer.RuntimeSymlinks)
	encoded, err := encodePolicyFingerprint(renderer, runner.Identity(), limits, warmupFixtureDigests())
	if err != nil {
		return Policy{}, err
	}
	return Policy{renderer: renderer, limits: limits, runner: runner,
		runnerID: runner.Identity(), fingerprint: digest(encoded)}, nil
}

func encodePolicyFingerprint(renderer Renderer, runnerID string, limits Limits, warmupDigests []string) ([]byte, error) {
	return canonical.Marshal(struct {
		Version          string           `json:"version"`
		Executable       string           `json:"executable"`
		ExecutableSHA256 string           `json:"executable_sha256"`
		RuntimeIdentity  string           `json:"runtime_identity"`
		RunnerIdentity   string           `json:"runner_identity"`
		Runtime          []RuntimeFile    `json:"runtime"`
		RuntimeSymlinks  []RuntimeSymlink `json:"runtime_symlinks"`
		WarmupContract   string           `json:"warmup_contract"`
		WarmupDigests    []string         `json:"warmup_digests"`
		Arguments        []string         `json:"arguments"`
		Environment      []string         `json:"environment"`
		Profiles         []string         `json:"profiles"`
		PrivateRoot      bool             `json:"private_root"`
		Limits           struct {
			MaxSourceBytes     int64 `json:"max_source_bytes"`
			MaxNormalizedBytes int64 `json:"max_normalized_bytes"`
			MaxPDFBytes        int64 `json:"max_pdf_bytes"`
			MaxWorkBytes       int64 `json:"max_work_bytes"`
			MaxXMLElements     int   `json:"max_xml_elements"`
			MaxXMLDepth        int   `json:"max_xml_depth"`
			MaxPages           int   `json:"max_pages"`
			MaxRuntimeEntries  int   `json:"max_runtime_entries"`
			Timeout            int64 `json:"timeout_ns"`
		} `json:"limits"`
	}{
		Version: ConverterVersion, Executable: renderer.Executable,
		ExecutableSHA256: renderer.ExecutableSHA256, RuntimeIdentity: renderer.RuntimeIdentity,
		RunnerIdentity: runnerID, Runtime: renderer.Runtime,
		RuntimeSymlinks: renderer.RuntimeSymlinks, Arguments: []string{
			"--headless", "--norestore", "--nolockcheck", "--nodefault", "--nofirststartwizard",
		}, Environment: libreOfficeEnvironment(), Profiles: []string{"docx->fodt"},
		WarmupContract: WarmupContractVersion, WarmupDigests: warmupDigests,
		PrivateRoot: true,
		Limits: struct {
			MaxSourceBytes     int64 `json:"max_source_bytes"`
			MaxNormalizedBytes int64 `json:"max_normalized_bytes"`
			MaxPDFBytes        int64 `json:"max_pdf_bytes"`
			MaxWorkBytes       int64 `json:"max_work_bytes"`
			MaxXMLElements     int   `json:"max_xml_elements"`
			MaxXMLDepth        int   `json:"max_xml_depth"`
			MaxPages           int   `json:"max_pages"`
			MaxRuntimeEntries  int   `json:"max_runtime_entries"`
			Timeout            int64 `json:"timeout_ns"`
		}{
			MaxSourceBytes: limits.MaxSourceBytes, MaxNormalizedBytes: limits.MaxNormalizedBytes,
			MaxPDFBytes: limits.MaxPDFBytes, MaxWorkBytes: limits.MaxWorkBytes,
			MaxXMLElements: limits.MaxXMLElements, MaxXMLDepth: limits.MaxXMLDepth,
			MaxPages: limits.MaxPages, MaxRuntimeEntries: limits.MaxRuntimeEntries,
			Timeout: int64(limits.Timeout),
		},
	})
}

func validateRuntimeEntries(renderer Renderer, maxEntries int) error {
	if len(renderer.Runtime)+len(renderer.RuntimeSymlinks) == 0 {
		return errors.New("renderer runtime is required")
	}
	if len(renderer.Runtime)+len(renderer.RuntimeSymlinks) > sandbox.MaxRuntimeEntries {
		return errors.New("renderer runtime exceeds entry limit")
	}
	if len(renderer.Runtime)+len(renderer.RuntimeSymlinks) > maxEntries {
		return errors.New("renderer runtime exceeds configured entry limit")
	}
	if err := (sandbox.Policy{
		Mode:       sandbox.SupervisedFileMode,
		Executable: renderer.Executable, ExecutableSHA256: renderer.ExecutableSHA256,
		Arguments: []string{"runtime"}, Environment: []string{"LANG=C"},
		MaxStdinBytes: 1, MaxStdoutBytes: 1,
		PrivateRoot: &sandbox.PrivateRoot{
			Runtime: renderer.Runtime, Symlinks: renderer.RuntimeSymlinks,
			RuntimeIdentity: renderer.RuntimeIdentity, WorkBytes: 1,
			InputName: "input", OutputName: "output", MaxOutputBytes: 1,
			WarmupInput: []byte("warmup"), WarmupInputSHA256: digest([]byte("warmup")),
		},
	}).Validate(); err != nil {
		return fmt.Errorf("renderer runtime is invalid: %w", err)
	}
	want, err := runtimeIdentityForManifest(renderer.Runtime, renderer.RuntimeSymlinks)
	if err != nil {
		return err
	}
	if want != renderer.RuntimeIdentity {
		return sandbox.ErrRuntimeIdentityMismatch
	}
	return nil
}

func validateLimits(limits Limits) error {
	ceiling := DefaultLimits()
	if limits.MaxSourceBytes <= 0 || limits.MaxSourceBytes > ceiling.MaxSourceBytes ||
		limits.MaxNormalizedBytes <= 0 || limits.MaxNormalizedBytes > ceiling.MaxNormalizedBytes ||
		limits.MaxPDFBytes <= 0 || limits.MaxPDFBytes > ceiling.MaxPDFBytes ||
		limits.MaxWorkBytes <= 0 || limits.MaxWorkBytes > ceiling.MaxWorkBytes ||
		limits.MaxXMLElements <= 0 || limits.MaxXMLElements > ceiling.MaxXMLElements ||
		limits.MaxXMLDepth <= 0 || limits.MaxXMLDepth > ceiling.MaxXMLDepth ||
		limits.MaxPages <= 0 || limits.MaxPages > ceiling.MaxPages ||
		limits.MaxRuntimeEntries <= 0 || limits.MaxRuntimeEntries > ceiling.MaxRuntimeEntries ||
		limits.Timeout <= 0 || limits.Timeout > ceiling.Timeout {
		return errors.New("render PDF limits must be positive and within DefaultLimits")
	}
	return nil
}

func (policy Policy) Fingerprint() string    { return policy.fingerprint }
func (policy Policy) Limits() Limits         { return policy.limits }
func (policy Policy) RunnerIdentity() string { return policy.runnerID }

func validateImmutableIdentity(value, subject string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return fmt.Errorf("%s must be an immutable sha256 identity", subject)
	}
	return validateSHA256(strings.TrimPrefix(value, "sha256:"), subject)
}

func validateSHA256(value, subject string) error {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", subject)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", subject)
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
