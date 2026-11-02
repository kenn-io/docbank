package openssh

import (
	"errors"
	"fmt"
)

var (
	ErrOpaqueProxyCommand    = errors.New("opaque ProxyCommand route")
	ErrNestedProxyRoute      = errors.New("nested proxy route")
	ErrInvalidProxyJump      = errors.New("invalid ProxyJump route")
	ErrPersistentUnsupported = errors.New("persistent OpenSSH connections are unsupported")
	ErrControlPathOccupied   = errors.New("OpenSSH control path is occupied")
	ErrProbeIndeterminate    = errors.New("OpenSSH control probe is indeterminate")
	ErrConnectionChanged     = errors.New("OpenSSH connection generation changed")
)

// ControlPathSecurityError reports a control entry that cannot safely be
// probed, adopted, or removed.
type ControlPathSecurityError struct {
	Path   string
	Reason string
	Err    error
}

func (e *ControlPathSecurityError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("OpenSSH control path %s is unsafe: %s: %v", e.Path, e.Reason, e.Err)
	}
	return fmt.Sprintf("OpenSSH control path %s is unsafe: %s", e.Path, e.Reason)
}

func (e *ControlPathSecurityError) Unwrap() error { return e.Err }

// ConfigError reports an invalid or incomplete SSH destination/configuration.
type ConfigError struct {
	Destination string
	Reason      string
}

func (e *ConfigError) Error() string {
	if e.Destination == "" {
		return "OpenSSH configuration: " + e.Reason
	}
	return fmt.Sprintf("OpenSSH configuration for %s: %s", e.Destination, e.Reason)
}

// RouteError identifies a route form that cannot be enumerated safely.
type RouteError struct {
	Target Target
	Kind   error
	Err    error
}

func (e *RouteError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("OpenSSH route for %s: %v: %v", e.Target.String(), e.Kind, e.Err)
	}
	return fmt.Sprintf("OpenSSH route for %s: %v", e.Target.String(), e.Kind)
}

func (e *RouteError) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Err}
}

// PathError reports a control-socket path that cannot be used safely.
type PathError struct {
	Path         string
	MaximumBytes int
	Reason       string
}

func (e *PathError) Error() string {
	if e.Path == "" {
		return "OpenSSH control path: " + e.Reason
	}
	if e.MaximumBytes <= 0 {
		return fmt.Sprintf("OpenSSH control path %s: %s", e.Path, e.Reason)
	}
	return fmt.Sprintf("OpenSSH control path %s: %s (maximum %d bytes)", e.Path, e.Reason, e.MaximumBytes)
}

// CommandError reports a failed OpenSSH operation with a stable exit code and
// diagnostic separate from the underlying process error.
type CommandError struct {
	Operation   string
	Destination string
	ExitCode    int
	Diagnostic  string
	Err         error
}

func (e *CommandError) Error() string {
	message := fmt.Sprintf("OpenSSH %s for %s failed", e.Operation, e.Destination)
	if e.ExitCode >= 0 {
		message += fmt.Sprintf(" with exit code %d", e.ExitCode)
	}
	if e.Diagnostic != "" {
		message += ": " + e.Diagnostic
	} else if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

func (e *CommandError) Unwrap() error { return e.Err }
