// Package daemonauth implements proof that a discovered loopback endpoint
// owns the private runtime record without sending either runtime secret to it.
package daemonauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

const (
	// ChallengePath is lifecycle plumbing and intentionally absent from OpenAPI.
	ChallengePath = "/api/daemon/challenge"
	// NonceBytes keeps every ownership challenge fresh and replay-resistant.
	NonceBytes  = 32
	proofDomain = "docbank-daemon-ownership-v1\x00"
)

// Proof returns the domain-separated HMAC for nonce using the per-run daemon
// token held in the owner-private runtime record.
func Proof(token string, nonce []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(proofDomain))
	_, _ = mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether proof is the expected HMAC without leaking comparison
// timing. Malformed hexadecimal is rejected.
func Verify(token string, nonce []byte, proof string) bool {
	got, err := hex.DecodeString(proof)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(Proof(token, nonce))
	return err == nil && hmac.Equal(got, want)
}
