package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	MaxExecutableBytes = int64(256 << 20)
	MaxResponseBytes   = int64(256 << 20)
	MaxControlBytes    = int64(64 << 10)
	MaxWorkBytes       = int64(512 << 20)
)

var (
	ErrUnavailable          = errors.New("sandbox policy unavailable")
	ErrCanceledBeforeLaunch = errors.New("sandbox canceled before launch")
	ErrOutputTooLarge       = errors.New("sandbox output exceeds limit")
	ErrChildFailed          = errors.New("sandbox child failed")
	ErrNormalRestart        = errors.New("sandbox child requested normal restart")
)

// Mode selects the process contract used by a request.
type Mode string

const (
	ExecMode           Mode = "exec"
	SupervisedFileMode Mode = "supervised-file"
)

// WorkDirectory names the two files exposed by a supervised request.
type WorkDirectory struct {
	InputName  string `json:"input_name,omitempty"`
	OutputName string `json:"output_name,omitempty"`
}

// Supervision describes optional file transport for one request.
type Supervision struct {
	Mode           Mode          `json:"mode,omitempty"`
	InputName      string        `json:"input_name,omitempty"`
	OutputName     string        `json:"output_name,omitempty"`
	Work           WorkDirectory `json:"work"`
	WorkBytes      int64         `json:"work_bytes,omitempty"`
	MaxOutputBytes int64         `json:"max_output_bytes,omitempty"`
}

// Policy is the complete fixed authority supplied to the launcher.
type Policy struct {
	Executable       string      `json:"executable"`
	ExecutableSHA256 string      `json:"executable_sha256"`
	Arguments        []string    `json:"arguments"`
	Environment      []string    `json:"environment"`
	Directory        string      `json:"directory"`
	ReadOnlyPaths    []string    `json:"read_only_paths,omitempty"`
	AllowLocalIPC    bool        `json:"allow_local_ipc,omitempty"`
	WorkBytes        int64       `json:"work_bytes,omitempty"`
	MaxStdinBytes    int64       `json:"max_stdin_bytes,omitempty"`
	MaxStdoutBytes   int64       `json:"max_stdout_bytes"`
	InputName        string      `json:"input_name,omitempty"`
	OutputName       string      `json:"output_name,omitempty"`
	Supervision      Supervision `json:"supervision"`
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
	LocalIPCAllowed      bool
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
	if !filepath.IsAbs(policy.Executable) || filepath.Clean(policy.Executable) != policy.Executable {
		return errors.New("sandbox executable must be an absolute clean path")
	}
	if err := validateSHA256(policy.ExecutableSHA256, "sandbox executable SHA-256"); err != nil {
		return err
	}
	if policy.Directory != "" && (!filepath.IsAbs(policy.Directory) || filepath.Clean(policy.Directory) != policy.Directory) {
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
	for _, value := range policy.ReadOnlyPaths {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, '\x00') {
			return errors.New("sandbox read-only path is invalid")
		}
	}
	if policy.WorkBytes < 0 || policy.WorkBytes > MaxWorkBytes ||
		policy.MaxStdinBytes <= 0 || policy.MaxStdinBytes > MaxResponseBytes ||
		policy.MaxStdoutBytes <= 0 || policy.MaxStdoutBytes > MaxResponseBytes {
		return errors.New("sandbox byte limits are outside the supported bounds")
	}
	supervision := policy.supervision()
	if supervision.Mode != ExecMode && supervision.Mode != SupervisedFileMode {
		return errors.New("sandbox mode is invalid")
	}
	if policy.AllowLocalIPC && supervision.Mode != SupervisedFileMode {
		return errors.New("sandbox local IPC is available only for supervised file mode")
	}
	if supervision.Mode == SupervisedFileMode {
		if err := validateWorkName(supervision.InputName, "input"); err != nil {
			return err
		}
		if err := validateWorkName(supervision.OutputName, "output"); err != nil {
			return err
		}
		if supervision.WorkBytes <= 0 || supervision.WorkBytes > MaxWorkBytes {
			return errors.New("sandbox supervised work limit is outside the supported bounds")
		}
		if supervision.MaxOutputBytes <= 0 || supervision.MaxOutputBytes > policy.MaxStdoutBytes {
			return errors.New("sandbox supervised output limit is outside the supported bounds")
		}
	}
	return nil
}

func (policy Policy) supervision() Supervision {
	value := policy.Supervision
	if value.Mode == "" {
		if policy.InputName != "" || policy.OutputName != "" || value.InputName != "" ||
			value.OutputName != "" || value.Work.InputName != "" || value.Work.OutputName != "" {
			value.Mode = SupervisedFileMode
		} else {
			value.Mode = ExecMode
		}
	}
	if value.InputName == "" {
		value.InputName = policy.InputName
	}
	if value.OutputName == "" {
		value.OutputName = policy.OutputName
	}
	if value.Work.InputName == "" {
		value.Work.InputName = value.InputName
	}
	if value.Work.OutputName == "" {
		value.Work.OutputName = value.OutputName
	}
	if value.InputName == "" {
		value.InputName = value.Work.InputName
	}
	if value.OutputName == "" {
		value.OutputName = value.Work.OutputName
	}
	if value.WorkBytes == 0 {
		value.WorkBytes = policy.WorkBytes
	}
	if value.MaxOutputBytes == 0 {
		value.MaxOutputBytes = policy.MaxStdoutBytes
	}
	if value.Mode == SupervisedFileMode && value.WorkBytes == 0 {
		value.WorkBytes = MaxWorkBytes
	}
	return value
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

func encodeControl(control launchControl) ([]byte, error) {
	encoded, err := json.Marshal(control)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox control: %w", err)
	}
	if len(encoded) == 0 || int64(len(encoded)) > MaxControlBytes {
		return nil, errors.New("sandbox control exceeds its byte limit")
	}
	return encoded, nil
}

func decodeControl(encoded []byte) (launchControl, error) {
	if len(encoded) == 0 || int64(len(encoded)) > MaxControlBytes {
		return launchControl{}, errors.New("sandbox control exceeds its byte limit")
	}
	var control launchControl
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return launchControl{}, errors.New("sandbox control is malformed")
	}
	if err := control.validate(); err != nil {
		return launchControl{}, err
	}
	control.Policy.Arguments = slices.Clone(control.Policy.Arguments)
	control.Policy.Environment = slices.Clone(control.Policy.Environment)
	control.Policy.ReadOnlyPaths = slices.Clone(control.Policy.ReadOnlyPaths)
	return control, nil
}

func validateWorkName(name, subject string) error {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name ||
		name == "." || strings.ContainsAny(name, "/\\:\x00") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("sandbox %s name is invalid", subject)
	}
	return nil
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
