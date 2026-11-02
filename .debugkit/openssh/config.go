// Package openssh provides application-neutral orchestration around an
// OpenSSH client. It intentionally leaves configuration files, credentials,
// host-key storage, and wire transport to OpenSSH itself.
package openssh

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Option is one normalized entry from ssh -G output. Names are lowercase;
// repeated options retain their original value order.
type Option struct {
	Name  string
	Value string
}

// EffectiveConfig is the security- and routing-relevant view of evaluated
// OpenSSH configuration plus the complete normalized option stream.
type EffectiveConfig struct {
	User                  string
	Hostname              string
	Port                  int
	HostKeyAlias          string
	StrictHostKeyChecking string
	ProxyJump             string
	ProxyCommand          string
	Options               []Option
}

// ParseConfig parses ssh -G output. Unrecognized options remain in Options so
// callers can bind identities to the complete effective configuration.
func ParseConfig(output []byte) EffectiveConfig {
	var config EffectiveConfig
	for line := range strings.Lines(string(output)) {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.ToLower(fields[0])
		value := strings.TrimSpace(line[len(fields[0]):])
		config.Options = append(config.Options, Option{Name: name, Value: value})
		switch name {
		case "user":
			config.User = value
		case "hostname":
			config.Hostname = value
		case "port":
			config.Port, _ = strconv.Atoi(value)
		case "hostkeyalias":
			config.HostKeyAlias = meaningfulOption(value)
		case "stricthostkeychecking":
			config.StrictHostKeyChecking = normalizeBooleanPolicy(value)
		case "proxyjump":
			config.ProxyJump = meaningfulOption(value)
		case "proxycommand":
			config.ProxyCommand = meaningfulOption(value)
		}
	}
	return config
}

// CanonicalOptions returns a deterministic representation suitable for
// identity construction. Option names are sorted while repeated values retain
// the order emitted by OpenSSH.
func (c EffectiveConfig) CanonicalOptions() []string {
	values := make(map[string][]string)
	for _, option := range c.Options {
		values[option.Name] = append(values[option.Name], option.Value)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	canonical := make([]string, 0, len(c.Options))
	for _, name := range names {
		for _, value := range values[name] {
			canonical = append(canonical, name+"="+value)
		}
	}
	return canonical
}

func meaningfulOption(value string) string {
	if strings.EqualFold(value, "none") {
		return ""
	}
	return value
}

func normalizeBooleanPolicy(value string) string {
	switch strings.ToLower(value) {
	case "true":
		return "yes"
	case "false":
		return "no"
	default:
		return strings.ToLower(value)
	}
}

// OutputRunner executes an argv and captures its output. It lets callers keep
// ownership of process limits, environment sanitization, and login-shell use.
type OutputRunner func(
	ctx context.Context,
	argv []string,
) (stdout, stderr []byte, exitCode int, err error)

// Resolver evaluates OpenSSH configuration for destinations.
type Resolver struct {
	Executable string
	BaseArgs   []string
	Run        OutputRunner
}

// Resolve evaluates target through ssh -G. Callers that require an account
// login shell should inject a Run implementation that preserves that boundary.
func (r Resolver) Resolve(ctx context.Context, target Target) (EffectiveConfig, error) {
	if err := ValidateTarget(target); err != nil {
		return EffectiveConfig{}, err
	}
	executable := r.Executable
	if executable == "" {
		executable = "ssh"
	}
	run := r.Run
	if run == nil {
		run = runOutput
	}
	argv := []string{executable, "-G"}
	if target.User != "" {
		argv = append(argv, "-l", target.User)
	}
	if target.Port != 0 {
		argv = append(argv, "-p", strconv.Itoa(target.Port))
	}
	argv = append(argv, r.BaseArgs...)
	argv = append(argv, "--", target.Hostname)
	stdout, stderr, exitCode, err := run(ctx, argv)
	if err != nil || exitCode != 0 {
		return EffectiveConfig{}, &CommandError{
			Operation:   "resolve configuration",
			Destination: target.String(),
			ExitCode:    exitCode,
			Diagnostic:  strings.TrimSpace(string(stderr)),
			Err:         err,
		}
	}
	config := ParseConfig(stdout)
	if config.Hostname == "" {
		return EffectiveConfig{}, &ConfigError{
			Destination: target.String(),
			Reason:      "ssh -G did not report a hostname",
		}
	}
	return config, nil
}

func runOutput(
	ctx context.Context,
	argv []string,
) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return []byte(stdout.String()), []byte(stderr.String()), -1,
				errors.Join(contextErr, err)
		}
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return []byte(stdout.String()), []byte(stderr.String()), -1, err
		}
		exitCode = exitErr.ExitCode()
		err = nil
	}
	return []byte(stdout.String()), []byte(stderr.String()), exitCode, err
}
