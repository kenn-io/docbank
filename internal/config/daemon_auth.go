package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

const (
	// DaemonChallengePath is lifecycle plumbing and absent from the running server's public OpenAPI.
	DaemonChallengePath = "/api/daemon/challenge"
	// DaemonKeyChallengePath lets a remote client check that an endpoint holds the
	// daemon API key before sending the key or document bytes to it.
	DaemonKeyChallengePath = "/api/daemon/key-challenge"
	// DaemonAuthNonceBytes keeps every ownership challenge fresh and replay-resistant.
	DaemonAuthNonceBytes = 32
	proofDomain          = "docbank-daemon-ownership-v1\x00"
	keyProofDomain       = "docbank-api-key-proof-v1\x00"
)

// DaemonAuthProof returns the domain-separated HMAC for nonce using the per-run daemon
// token held in the owner-private runtime record.
func DaemonAuthProof(token string, nonce []byte) string {
	return proof(proofDomain, token, nonce)
}

// VerifyDaemonAuthProof reports whether proof is the expected HMAC without leaking comparison
// timing. Malformed hexadecimal is rejected.
func VerifyDaemonAuthProof(token string, nonce []byte, proof string) bool {
	return verify(proofDomain, token, nonce, proof)
}

// DaemonKeyProof returns the domain-separated HMAC for nonce using the daemon API key.
func DaemonKeyProof(key string, nonce []byte) string {
	return proof(keyProofDomain, key, nonce)
}

// VerifyDaemonKeyProof reports whether proof shows possession of the API key for nonce.
func VerifyDaemonKeyProof(key string, nonce []byte, proof string) bool {
	return verify(keyProofDomain, key, nonce, proof)
}

func proof(domain, secret string, nonce []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

func verify(domain, secret string, nonce []byte, got string) bool {
	decoded, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(proof(domain, secret, nonce))
	return err == nil && hmac.Equal(decoded, want)
}
