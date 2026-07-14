package daemonauth

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProofVerification(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x42}, NonceBytes)
	proof := Proof("runtime-secret", nonce)

	assert.True(t, Verify("runtime-secret", nonce, proof))
	assert.False(t, Verify("wrong-secret", nonce, proof))
	assert.False(t, Verify("runtime-secret", append([]byte{0x00}, nonce...), proof))
	assert.False(t, Verify("runtime-secret", nonce, "not-hex"))
}
