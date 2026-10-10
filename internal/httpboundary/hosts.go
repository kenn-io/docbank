// Package httpboundary validates HTTP authorities without DNS resolution.
package httpboundary

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Authority is a normalized HTTP host with an optional explicit port.
type Authority struct {
	Host string
	Port string
}

// Parse accepts an IP or ASCII DNS name, optionally with a port. IPv6
// authorities require brackets; zone identifiers, URLs and wildcards fail.
func Parse(value string) (Authority, bool) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/@?#\\%") {
		return Authority{}, false
	}
	host, port := value, ""
	if strings.Contains(value, ":") {
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil {
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				host = value[1 : len(value)-1]
			} else {
				return Authority{}, false
			}
		} else {
			n, err := strconv.ParseUint(port, 10, 16)
			if err != nil || n == 0 {
				return Authority{}, false
			}
			port = strconv.FormatUint(n, 10)
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || ip.Unmap().IsUnspecified() {
			return Authority{}, false
		}
		return Authority{Host: ip.String(), Port: port}, true
	}
	host = strings.ToLower(host)
	if len(host) > 253 {
		return Authority{}, false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return Authority{}, false
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return Authority{}, false
			}
		}
	}
	return Authority{Host: host, Port: port}, true
}

// ValidateHosts rejects malformed entries rather than silently ignoring them.
func ValidateHosts(hosts []string) error {
	for _, host := range hosts {
		if _, ok := Parse(host); !ok {
			return fmt.Errorf("invalid allowed host %q: use an IP or DNS name, optionally with a port", host)
		}
	}
	return nil
}

// Allowed permits loopback authorities, the concrete listen IP, and explicit
// hosts. A wildcard listen address never grants arbitrary Host values.
func Allowed(value, listenHost string, hosts []string) bool {
	authority, ok := Parse(value)
	if !ok {
		return false
	}
	if authority.Host == "localhost" {
		return true
	}
	requestedIP, requestedErr := netip.ParseAddr(authority.Host)
	if requestedErr == nil && requestedIP.Unmap().IsLoopback() {
		return true
	}
	if ip, err := netip.ParseAddr(listenHost); err == nil && !ip.Unmap().IsUnspecified() && requestedErr == nil && requestedIP.Unmap() == ip.Unmap() {
		return true
	}
	for _, host := range hosts {
		allowed, valid := Parse(host)
		if valid && allowed.Host == authority.Host && (allowed.Port == "" || allowed.Port == authority.Port) {
			return true
		}
	}
	return false
}

// Same compares HTTP authorities, treating omitted ports as port 80.
// IPv4 and IPv4-mapped IPv6 remain distinct origins even when they reach
// the same address.
func Same(a, b string) bool {
	left, leftOK := Parse(a)
	right, rightOK := Parse(b)
	if left.Port == "" {
		left.Port = "80"
	}
	if right.Port == "" {
		right.Port = "80"
	}
	return leftOK && rightOK && left == right
}
