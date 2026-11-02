package embedconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CanonicalEndpoint returns the identity and request form of raw.
// The result uses a lowercase scheme and host, drops the default port, and
// drops a trailing slash. An IPv6 zone is preserved as written, not
// lowercased, and each percent sign in it is encoded as %25.
// Userinfo, query, and fragment are rejected so a credential or a
// cache-buster cannot hide inside the endpoint.
// Plaintext HTTP is limited to loopback unless trustPrivateNetwork is set.
// With that opt-in, private, link-local, unspecified, and carrier-grade NAT
// addresses are allowed, and so is a DNS name. A name is not classified as
// public or private. The opt-in is the caller's statement that the name is
// on a network they trust. Without the opt-in, a name other than localhost
// is rejected. The zone is removed before the address check.
// A parent step is a decoded path piece that is exactly "..". A name that
// contains two dots, such as "v1..2", is kept.
func CanonicalEndpoint(raw string, trustPrivateNetwork bool) (string, error) {
	parsed, err := parseEndpoint(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("embed endpoint is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("embed endpoint must use http or https")
	}
	addr, _, ok := endpointHost(parsed)
	if !ok {
		return "", errors.New("embed endpoint must include a host")
	}
	if parsed.User != nil {
		return "", errors.New("embed endpoint must not include userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("embed endpoint must not include a query or fragment")
	}
	if pathHasParentSegment(parsed.Path) {
		return "", errors.New("embed endpoint path must not contain a parent segment")
	}
	if parsed.Scheme == "http" && !plaintextAllowed(addr, trustPrivateNetwork) {
		return "", errors.New("embed plaintext http requires loopback or an explicitly trusted private address")
	}
	return schemeHost(parsed) + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

// pathHasParentSegment reports a decoded path piece that is exactly "..".
// A name that merely contains two dots, such as "v1..2", is kept.
func pathHasParentSegment(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Origin returns the canonical scheme and host of u, without a path.
func Origin(u *url.URL) (string, error) {
	if u == nil || u.Scheme == "" {
		return "", errors.New("embed endpoint origin is missing")
	}
	if _, _, ok := endpointHost(u); !ok {
		return "", errors.New("embed endpoint origin is missing")
	}
	return schemeHost(u), nil
}

// parseEndpoint accepts the RFC 6874 zone form and the common bare-percent
// form, such as https://[fe80::1%eth0]/v1, which url.Parse rejects.
func parseEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err == nil {
		return parsed, nil
	}
	fixed := encodeBareIPv6Zone(raw)
	if fixed == raw {
		return nil, err
	}
	parsed, retryErr := url.Parse(fixed)
	if retryErr != nil {
		return nil, err
	}
	return parsed, nil
}

// encodeBareIPv6Zone rewrites a bare '%' zone separator in the authority to
// %25. A zone that is already introduced by %25 is left alone.
func encodeBareIPv6Zone(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	auth := rest
	suffix := ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		auth = rest[:i]
		suffix = rest[i:]
	}
	userinfo := ""
	host := auth
	if at := strings.LastIndex(auth, "@"); at >= 0 {
		userinfo = auth[:at+1]
		host = auth[at+1:]
	}
	if !strings.HasPrefix(host, "[") {
		return raw
	}
	end := strings.IndexByte(host, ']')
	if end < 0 {
		return raw
	}
	literal := host[1:end]
	if strings.Contains(literal, "%25") || !strings.Contains(literal, "%") {
		return raw
	}
	addr, zone, _ := strings.Cut(literal, "%")
	if !strings.Contains(addr, ":") {
		return raw
	}
	literal = addr + "%25" + strings.ReplaceAll(zone, "%", "%25")
	return scheme + "://" + userinfo + "[" + literal + host[end:] + suffix
}

// endpointHost splits the address from an IPv6 zone.
// net.SplitHostPort requires a port, so a portless host uses Hostname.
// Both return the zone already unescaped: the separator is '%', not the
// characters '%25'. A non-empty Host such as ":443" can still have an empty
// hostname and is not usable.
func endpointHost(u *url.URL) (addr, zone string, ok bool) {
	if u == nil {
		return "", "", false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Hostname()
	}
	if host == "" {
		return "", "", false
	}
	before, after, found := strings.Cut(host, "%")
	if found && strings.Contains(before, ":") {
		if before == "" {
			return "", "", false
		}
		return before, after, true
	}
	return host, "", true
}

func schemeHost(u *url.URL) string {
	addr, zone, ok := endpointHost(u)
	if !ok {
		return ""
	}
	host := strings.ToLower(addr)
	if zone != "" {
		// Keep the zone text, including its case. Encode every percent so
		// the result is a valid URL instead of an invalid escape.
		host += "%25" + strings.ReplaceAll(zone, "%", "%25")
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return strings.ToLower(u.Scheme) + "://" + host
}

func plaintextAllowed(host string, trustPrivateNetwork bool) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return trustPrivateNetwork
	}
	if ip.IsLoopback() {
		return true
	}
	if !trustPrivateNetwork {
		return false
	}
	return ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && ip[0] == 100 && ip[1]&0xc0 == 0x40
}
