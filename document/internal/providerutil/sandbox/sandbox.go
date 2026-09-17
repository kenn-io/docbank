package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	MaxExecutableBytes         = int64(256 << 20)
	MaxResponseBytes           = int64(256 << 20)
	MaxExecControlBytes        = int64(64 << 10)
	MaxPrivateRootControlBytes = int64(8 << 20)
	MaxWarmupBytes             = int64(1 << 20)
	MaxWorkBytes               = int64(512 << 20)
	MaxRuntimeEntries          = 250_000
	MaxRuntimeDepth            = 64
	MaxRuntimeBytes            = int64(4 << 30)
)

var (
	ErrUnavailable             = errors.New("sandbox policy unavailable")
	ErrPrivateRootUnavailable  = errors.New("sandbox private root unavailable")
	ErrRuntimeSpecialFile      = errors.New("sandbox runtime contains a special file")
	ErrRuntimeIdentityMismatch = errors.New("sandbox runtime identity mismatch")
	ErrCanceledBeforeLaunch    = errors.New("sandbox canceled before launch")
	ErrOutputTooLarge          = errors.New("sandbox output exceeds limit")
	ErrChildFailed             = errors.New("sandbox child failed")
)

// Mode selects the process contract used by a request.
type Mode string

const (
	ExecMode           Mode = "exec"
	SupervisedFileMode Mode = "supervised-file"
)

// RuntimeFile identifies one regular file attached to a private root.
type RuntimeFile struct {
	SourcePath string `json:"source_path"`
	GuestPath  string `json:"guest_path"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
}

// RuntimeSymlink identifies one relative link created inside a private root.
type RuntimeSymlink struct {
	GuestPath string `json:"guest_path"`
	Target    string `json:"target"`
}

// PrivateRoot describes the complete supervised filesystem authority.
type PrivateRoot struct {
	Runtime           []RuntimeFile    `json:"runtime"`
	Symlinks          []RuntimeSymlink `json:"symlinks,omitempty"`
	RuntimeIdentity   string           `json:"runtime_identity"`
	WorkBytes         int64            `json:"work_bytes"`
	InputName         string           `json:"input_name"`
	OutputName        string           `json:"output_name"`
	MaxOutputBytes    int64            `json:"max_output_bytes"`
	WarmupInput       []byte           `json:"warmup_input"`
	WarmupInputSHA256 string           `json:"warmup_input_sha256"`
}

// Policy is the complete fixed authority supplied to the launcher.
type Policy struct {
	Mode             Mode         `json:"mode"`
	Executable       string       `json:"executable"`
	ExecutableSHA256 string       `json:"executable_sha256"`
	Arguments        []string     `json:"arguments"`
	Environment      []string     `json:"environment"`
	Directory        string       `json:"directory"`
	PrivateRoot      *PrivateRoot `json:"private_root,omitempty"`
	MaxStdinBytes    int64        `json:"max_stdin_bytes"`
	MaxStdoutBytes   int64        `json:"max_stdout_bytes"`
}

// Request carries one bounded input and its authenticated policy identity.
type Request struct {
	PolicyFingerprint string `json:"policy_fingerprint,omitempty"`
	Policy            Policy `json:"policy"`
	Stdin             []byte `json:"-"`
	StdinSHA256       string `json:"stdin_sha256"`
}

type ExecRequest = Request
type SupervisedRequest = Request
type RunRequest = Request

// Attestation records the controls applied to a completed run.
type Attestation struct {
	RunnerIdentity       string
	PolicyFingerprint    string
	ExecutableSHA256     string
	StdinSHA256          string
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

// Result owns the bounded bytes returned by a sandbox run.
type Result struct {
	Stdout      []byte
	Output      []byte
	Attestation Attestation
}

type launchControl struct {
	Policy            Policy `json:"policy"`
	PolicyFingerprint string `json:"policy_fingerprint,omitempty"`
	StdinSHA256       string `json:"stdin_sha256"`
}

// Validate checks the portable parts of a launch policy in the parent.
func (policy Policy) Validate() error {
	if policy.Mode != ExecMode && policy.Mode != SupervisedFileMode {
		return errors.New("sandbox mode is invalid")
	}
	if !filepath.IsAbs(policy.Executable) || filepath.Clean(policy.Executable) != policy.Executable {
		return errors.New("sandbox executable must be an absolute clean path")
	}
	if err := validateSHA256(policy.ExecutableSHA256, "sandbox executable SHA-256"); err != nil {
		return err
	}
	if policy.Directory != "" &&
		(!filepath.IsAbs(policy.Directory) || filepath.Clean(policy.Directory) != policy.Directory) {
		return errors.New("sandbox directory must be an absolute clean path")
	}
	if len(policy.Arguments) == 0 {
		return errors.New("sandbox arguments are required")
	}
	for _, value := range policy.Arguments {
		if strings.ContainsRune(value, '\x00') {
			return errors.New("sandbox arguments contain NUL")
		}
	}
	for _, value := range policy.Environment {
		if strings.ContainsRune(value, '\x00') || !strings.ContainsRune(value, '=') {
			return errors.New("sandbox environment entry is invalid")
		}
	}
	if policy.MaxStdinBytes <= 0 || policy.MaxStdinBytes > MaxResponseBytes ||
		policy.MaxStdoutBytes <= 0 || policy.MaxStdoutBytes > MaxResponseBytes {
		return errors.New("sandbox byte limits are outside the supported bounds")
	}
	if policy.Mode == ExecMode {
		if policy.PrivateRoot != nil {
			return errors.New("sandbox private root is only valid for supervised mode")
		}
		return nil
	}
	if policy.PrivateRoot == nil {
		return errors.New("sandbox supervised mode requires a private root")
	}
	root := policy.PrivateRoot
	if err := validateRuntime(root); err != nil {
		return err
	}
	if root.WorkBytes <= 0 || root.WorkBytes > MaxWorkBytes {
		return errors.New("sandbox supervised work limit is outside the supported bounds")
	}
	if root.MaxOutputBytes <= 0 || root.MaxOutputBytes > policy.MaxStdoutBytes {
		return errors.New("sandbox supervised output limit is outside the supported bounds")
	}
	if err := validateWorkName(root.InputName, "input"); err != nil {
		return err
	}
	if err := validateWorkName(root.OutputName, "output"); err != nil {
		return err
	}
	if root.InputName == root.OutputName {
		return errors.New("sandbox supervised input and output names must differ")
	}
	if len(root.WarmupInput) == 0 || int64(len(root.WarmupInput)) > MaxWarmupBytes {
		return errors.New("sandbox supervised warm-up input is outside the supported bound")
	}
	if err := validateSHA256(root.WarmupInputSHA256, "sandbox warm-up input SHA-256"); err != nil {
		return err
	}
	warmupDigest := sha256.Sum256(root.WarmupInput)
	if hex.EncodeToString(warmupDigest[:]) != root.WarmupInputSHA256 {
		return errors.New("sandbox warm-up input SHA-256 does not match content")
	}
	return nil
}

func validateRuntime(root *PrivateRoot) error {
	if err := validateRuntimeIdentity(root.RuntimeIdentity); err != nil {
		return err
	}
	if len(root.Runtime)+len(root.Symlinks) > MaxRuntimeEntries {
		return errors.New("sandbox runtime entry count exceeds limit")
	}
	seen := make(map[string]struct{}, len(root.Runtime)+len(root.Symlinks))
	for _, file := range root.Runtime {
		if err := validateRuntimePath(file.SourcePath, true); err != nil {
			return err
		}
		if err := validateRuntimePath(file.GuestPath, false); err != nil {
			return err
		}
		if err := validateSHA256(file.SHA256, "sandbox runtime SHA-256"); err != nil {
			return err
		}
		if runtimePathDepth(file.GuestPath) > MaxRuntimeDepth {
			return errors.New("sandbox runtime path depth exceeds limit")
		}
		if _, ok := seen[file.GuestPath]; ok {
			return errors.New("sandbox runtime guest paths must be unique")
		}
		seen[file.GuestPath] = struct{}{}
	}
	for _, link := range root.Symlinks {
		if err := validateRuntimePath(link.GuestPath, false); err != nil {
			return err
		}
		if err := validateSymlinkTarget(link.Target); err != nil {
			return err
		}
		if runtimePathDepth(link.GuestPath) > MaxRuntimeDepth {
			return errors.New("sandbox runtime path depth exceeds limit")
		}
		if _, ok := seen[link.GuestPath]; ok {
			return errors.New("sandbox runtime guest paths must be unique")
		}
		seen[link.GuestPath] = struct{}{}
	}
	for index := 1; index < len(root.Runtime); index++ {
		if root.Runtime[index-1].GuestPath > root.Runtime[index].GuestPath {
			return errors.New("sandbox runtime files must be sorted by guest path")
		}
	}
	for index := 1; index < len(root.Symlinks); index++ {
		if root.Symlinks[index-1].GuestPath > root.Symlinks[index].GuestPath {
			return errors.New("sandbox runtime symlinks must be sorted by guest path")
		}
	}
	return nil
}

func runtimePathDepth(value string) int {
	trimmed := strings.Trim(value, "/")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "/")
}

func validateRuntimePath(path string, source bool) error {
	var absolute bool
	var clean string
	if source {
		absolute = filepath.IsAbs(path)
		clean = filepath.Clean(path)
	} else {
		absolute = pathpkg.IsAbs(path)
		clean = pathpkg.Clean(path)
	}
	if path == "" || !absolute || clean != path ||
		strings.ContainsRune(path, '\x00') {
		if source {
			return errors.New("sandbox runtime source path must be absolute and clean")
		}
		return errors.New("sandbox runtime guest path must be absolute and clean")
	}
	return nil
}

func validateSymlinkTarget(target string) error {
	if target == "" || pathpkg.IsAbs(target) || pathpkg.Clean(target) != target ||
		strings.ContainsRune(target, '\x00') {
		return errors.New("sandbox runtime symlink target must be relative and clean")
	}
	if slices.Contains(strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
		return errors.New("sandbox runtime symlink target escapes its root")
	}
	return nil
}

func validateRuntimeIdentity(value string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return errors.New("sandbox runtime identity must be an immutable sha256 identity")
	}
	return validateSHA256(strings.TrimPrefix(value, "sha256:"), "sandbox runtime identity")
}

func validateWorkName(name, subject string) error {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name ||
		name == "." || strings.ContainsAny(name, "/\\:\x00") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("sandbox %s name is invalid", subject)
	}
	return nil
}

func (request Request) validate() error {
	if err := request.Policy.Validate(); err != nil {
		return err
	}
	if len(request.Stdin) == 0 || int64(len(request.Stdin)) > request.Policy.MaxStdinBytes {
		return errors.New("sandbox input is outside the configured bound")
	}
	digest := sha256.Sum256(request.Stdin)
	if err := validateSHA256(request.StdinSHA256, "sandbox input SHA-256"); err != nil ||
		hex.EncodeToString(digest[:]) != request.StdinSHA256 {
		return errors.New("sandbox input SHA-256 does not match content")
	}
	return nil
}

func (control launchControl) validate() error {
	if err := control.Policy.Validate(); err != nil {
		return err
	}
	return validateSHA256(control.StdinSHA256, "sandbox input SHA-256")
}

func controlLimit(mode Mode) int64 {
	if mode == SupervisedFileMode {
		return MaxPrivateRootControlBytes
	}
	return MaxExecControlBytes
}

func encodeControl(control launchControl) ([]byte, error) {
	encoded, err := json.Marshal(control)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox control: %w", err)
	}
	if len(encoded) == 0 || int64(len(encoded)) > controlLimit(control.Policy.Mode) {
		return nil, errors.New("sandbox control exceeds its byte limit")
	}
	return encoded, nil
}

func decodeControl(encoded []byte) (launchControl, error) {
	if len(encoded) == 0 || int64(len(encoded)) > MaxPrivateRootControlBytes {
		return launchControl{}, errors.New("sandbox control exceeds its byte limit")
	}
	var control launchControl
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return launchControl{}, errors.New("sandbox control is malformed")
	}
	if int64(len(encoded)) > controlLimit(control.Policy.Mode) {
		return launchControl{}, errors.New("sandbox control exceeds its mode byte limit")
	}
	if err := control.validate(); err != nil {
		return launchControl{}, err
	}
	control.Policy.Arguments = slices.Clone(control.Policy.Arguments)
	control.Policy.Environment = slices.Clone(control.Policy.Environment)
	if control.Policy.PrivateRoot != nil {
		control.Policy.PrivateRoot.Runtime = slices.Clone(control.Policy.PrivateRoot.Runtime)
		control.Policy.PrivateRoot.Symlinks = slices.Clone(control.Policy.PrivateRoot.Symlinks)
		control.Policy.PrivateRoot.WarmupInput = slices.Clone(control.Policy.PrivateRoot.WarmupInput)
	}
	return control, nil
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
