package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/httpboundary"
)

// minPublicAPIKeyLength keeps a network-exposed key out of online-guessing
// range without a lockout that an unauthenticated client could trigger.
const minPublicAPIKeyLength = 32

// WebSessionLifetime bounds key-login browser authority. Fragment sessions keep
// their existing daemon-lifetime behavior.
func (c WebConfig) WebSessionLifetime() time.Duration {
	if c.SessionLifetime == 0 {
		return 24 * time.Hour
	}
	return c.SessionLifetime.Std()
}

// CanonicalWebOrigin matches the authority serialization used by browsers.
func CanonicalWebOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.Contains(value, "#") || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("[web] public_origin must be a root HTTP or HTTPS origin without path, credentials, query or fragment")
	}
	host, err := WebHostAuthority(u.Host)
	if err != nil {
		return "", err
	}
	u.Host = host
	if (u.Port() == "80" && u.Scheme == "http") || (u.Port() == "443" && u.Scheme == "https") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String(), nil
}

// WebHostAuthority validates an exact host[:port], without patterns or a scheme.
func WebHostAuthority(value string) (string, error) {
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r <= 32 || r >= 127 || strings.ContainsRune("/*?#@%\\", r) }) != -1 {
		return "", errors.New("[web] allowed_hosts entries must be exact host authorities")
	}
	u, err := url.Parse("http://" + value)
	if err != nil || u.Host != value || u.Hostname() == "" || u.User != nil || u.Path != "" {
		return "", errors.New("[web] invalid host authority")
	}
	if strings.Contains(u.Hostname(), ":") && net.ParseIP(u.Hostname()) == nil {
		return "", errors.New("[web] invalid IPv6 authority")
	}
	if strings.HasSuffix(value, ":") {
		return "", errors.New("[web] empty host port")
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("[web] invalid host port")
		}
		port = strconv.Itoa(number)
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); strings.Contains(host, ":") && ipv4 != nil {
			// Browsers serialize IPv4-mapped IPv6 literals as hexadecimal groups.
			host = fmt.Sprintf("::ffff:%x:%x", uint16(ipv4[0])<<8|uint16(ipv4[1]), uint16(ipv4[2])<<8|uint16(ipv4[3]))
		} else {
			host = ip.String()
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	// The server Host allowlist parses requests with httpboundary, so reject
	// authorities it cannot match, such as a trailing DNS dot or underscore.
	if _, ok := httpboundary.Parse(host); !ok {
		return "", fmt.Errorf("[web] host %q is not a supported IP or DNS authority", value)
	}
	return host, nil
}

func validateWebConfig(c Config) error {
	w := c.Web
	for _, host := range w.AllowedHosts {
		if _, err := WebHostAuthority(host); err != nil {
			return err
		}
	}
	if w.WebSessionLifetime() < time.Minute || w.WebSessionLifetime() > 90*24*time.Hour {
		return errors.New("[web] session_lifetime must be between 1m and 2160h")
	}
	if w.PublicOrigin == "" {
		if len(w.AllowedHosts) > 0 || w.TrustPrivateNetwork {
			return errors.New("[web] allowed_hosts and trust_private_network require public_origin")
		}
		return nil
	}
	if !w.Enabled || c.Server.APIKey == "" {
		return errors.New("[web] public_origin requires enabled = true and a configured server.api_key")
	}
	if len(c.Server.APIKey) < minPublicAPIKeyLength {
		return fmt.Errorf("[web] public_origin requires a server.api_key of at least %d characters; "+
			"generate one with `openssl rand -hex 32`", minPublicAPIKeyLength)
	}
	origin, err := CanonicalWebOrigin(w.PublicOrigin)
	if err != nil {
		return err
	}
	u, _ := url.Parse(origin)
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) && !w.TrustPrivateNetwork {
		return errors.New("[web] non-loopback HTTP public_origin requires trust_private_network = true or HTTPS")
	}
	return nil
}
