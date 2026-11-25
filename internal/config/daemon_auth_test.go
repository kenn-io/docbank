package config

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProofVerification(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x42}, DaemonAuthNonceBytes)
	proof := DaemonAuthProof("runtime-secret", nonce)

	assert.True(t, VerifyDaemonAuthProof("runtime-secret", nonce, proof))
	assert.False(t, VerifyDaemonAuthProof("wrong-secret", nonce, proof))
	assert.False(t, VerifyDaemonAuthProof("runtime-secret", append([]byte{0x00}, nonce...), proof))
	assert.False(t, VerifyDaemonAuthProof("runtime-secret", nonce, "not-hex"))
}

func TestKeyProofIsDomainSeparatedFromOwnershipProof(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x42}, DaemonAuthNonceBytes)
	proof := DaemonKeyProof("api-key", nonce)

	assert.True(t, VerifyDaemonKeyProof("api-key", nonce, proof))
	assert.False(t, VerifyDaemonKeyProof("other-key", nonce, proof))
	assert.False(t, VerifyDaemonAuthProof("api-key", nonce, proof), "an API key proof is not an ownership proof")
	assert.False(t, VerifyDaemonKeyProof("api-key", nonce, DaemonAuthProof("api-key", nonce)))
}
