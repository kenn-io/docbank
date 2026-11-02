package openssh

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Target is an OpenSSH destination with an optional explicit port. Package
// operations validate it with ValidateTarget before invoking OpenSSH.
type Target struct {
	User     string
	Hostname string
	Port     int
}

// ParseTarget accepts OpenSSH [user@]host, [user@]host:port, bracketed IPv6,
// and case-insensitive ssh:// URI destinations.
func ParseTarget(value string) (Target, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Target{}, &ConfigError{Reason: "empty SSH destination"}
	}
	if len(value) >= len("ssh://") && strings.EqualFold(value[:len("ssh://")], "ssh://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" || parsed.Path != "" ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return Target{}, &ConfigError{Reason: "invalid SSH URI"}
		}
		port, err := parsePort(parsed.Port())
		if err != nil {
			return Target{}, &ConfigError{Reason: err.Error()}
		}
		user := ""
		if parsed.User != nil {
			user = parsed.User.Username()
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return Target{}, &ConfigError{Reason: "SSH URI must not contain a password"}
			}
		}
		target := Target{User: user, Hostname: parsed.Hostname(), Port: port}
		if err := ValidateTarget(target); err != nil {
			return Target{}, err
		}
		return target, nil
	}

	user := ""
	hostPort := value
	if at := strings.LastIndex(value, "@"); at >= 0 {
		user = value[:at]
		hostPort = value[at+1:]
		if user == "" {
			return Target{}, &ConfigError{Destination: value, Reason: "empty SSH user"}
		}
	}
	host, port, err := splitHostPort(hostPort)
	if err != nil || host == "" {
		reason := "empty SSH hostname"
		if err != nil {
			reason = err.Error()
		}
		return Target{}, &ConfigError{Destination: value, Reason: reason}
	}
	target := Target{User: user, Hostname: host, Port: port}
	if err := ValidateTarget(target); err != nil {
		return Target{}, err
	}
	return target, nil
}

// ValidateTarget rejects destination components that OpenSSH configuration
// token expansion could reinterpret as local shell syntax. Unusual account
// names should be selected through a trusted ssh_config host alias instead of
// supplied programmatically.
func ValidateTarget(target Target) error {
	destination := target.String()
	if target.User != "" && !safeTargetComponent(target.User) {
		return &ConfigError{Destination: destination, Reason: "unsafe SSH user"}
	}
	if target.Hostname == "" {
		return &ConfigError{Destination: destination, Reason: "empty SSH hostname"}
	}
	if net.ParseIP(target.Hostname) == nil && !safeTargetComponent(target.Hostname) {
		return &ConfigError{Destination: destination, Reason: "unsafe SSH hostname"}
	}
	if target.Port < 0 || target.Port > 65535 {
		return &ConfigError{Destination: destination, Reason: "invalid SSH port"}
	}
	return nil
}

func safeTargetComponent(value string) bool {
	if value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '.', '_', '-':
			continue
		default:
			return false
		}
	}
	return value != "" && value[0] != '-'
}

func splitHostPort(value string) (string, int, error) {
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 {
			return "", 0, errors.New("unclosed IPv6 address")
		}
		host := value[1:end]
		remainder := value[end+1:]
		if remainder == "" {
			return host, 0, nil
		}
		if !strings.HasPrefix(remainder, ":") {
			return "", 0, errors.New("invalid bracketed SSH destination")
		}
		portValue := strings.TrimPrefix(remainder, ":")
		if portValue == "" {
			return "", 0, errors.New("invalid SSH port")
		}
		port, err := parsePort(portValue)
		return host, port, err
	}
	if strings.Count(value, ":") == 1 {
		host, portValue, _ := strings.Cut(value, ":")
		if portValue == "" {
			return "", 0, errors.New("invalid SSH port")
		}
		port, err := parsePort(portValue)
		if err != nil {
			return "", 0, err
		}
		return host, port, nil
	}
	return value, 0, nil
}

func parsePort(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("invalid SSH port")
	}
	return port, nil
}

func (t Target) String() string {
	host := t.Hostname
	if t.Port != 0 {
		host = net.JoinHostPort(host, strconv.Itoa(t.Port))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.User != "" {
		host = t.User + "@" + host
	}
	return host
}

// CommandDestination returns the destination argument and optional -p value
// separately, matching ssh(1)'s command-line grammar.
func (t Target) CommandDestination() (string, int) {
	host := t.Hostname
	if t.User != "" {
		host = t.User + "@" + host
	}
	return host, t.Port
}

// ResolvedTarget binds a logical destination to one effective OpenSSH config.
type ResolvedTarget struct {
	Target Target
	Config EffectiveConfig
}

// Route lists ProxyJump hops in connection order followed by the endpoint.
type Route []ResolvedTarget

// ConfigProvider resolves one route target.
type ConfigProvider func(context.Context, Target) (EffectiveConfig, error)

// ResolveRoute resolves a direct ProxyJump list. Opaque ProxyCommand routes and
// jump hosts that introduce another proxy route are rejected because callers
// cannot apply policy independently to every hop.
func ResolveRoute(
	ctx context.Context,
	endpoint Target,
	provider ConfigProvider,
) (Route, error) {
	if err := ValidateTarget(endpoint); err != nil {
		return nil, err
	}
	endpointConfig, err := provider(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if endpointConfig.ProxyCommand != "" {
		return nil, &RouteError{Target: endpoint, Kind: ErrOpaqueProxyCommand}
	}
	route := make(Route, 0, 1)
	if endpointConfig.ProxyJump != "" {
		for value := range strings.SplitSeq(endpointConfig.ProxyJump, ",") {
			hop, err := ParseTarget(value)
			if err != nil {
				return nil, &RouteError{Target: endpoint, Kind: ErrInvalidProxyJump, Err: err}
			}
			hopConfig, err := provider(ctx, hop)
			if err != nil {
				return nil, err
			}
			if hopConfig.ProxyCommand != "" || hopConfig.ProxyJump != "" {
				return nil, &RouteError{Target: hop, Kind: ErrNestedProxyRoute}
			}
			route = append(route, ResolvedTarget{Target: hop, Config: hopConfig})
		}
	}
	return append(route, ResolvedTarget{Target: endpoint, Config: endpointConfig}), nil
}
