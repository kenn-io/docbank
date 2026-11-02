package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strconv"
)

const (
	proofNonceSize       = 32
	proofChallengeHeader = "X-Kit-Daemon-Challenge"
	proofResponseHeader  = "X-Kit-Daemon-Proof"
	proofDomain          = "go.kenn.io/kit/daemon/proof/v1"
)

var proofEncoding = base64.RawURLEncoding.Strict()

// Proof owns the shared secret used to prove a daemon runtime identity without
// sending the secret to a candidate endpoint. Construct one with NewProof.
type Proof struct { //nolint:recvcheck // Format keeps a value receiver so both Proof and *Proof redact when printed
	key []byte
}

// NewProof copies key into an opaque proof capability.
func NewProof(key []byte) (*Proof, error) {
	if len(key) == 0 {
		return nil, errors.New("daemon proof key is empty")
	}
	return &Proof{key: bytes.Clone(key)}, nil
}

// Format redacts the proof key from diagnostic formatting.
func (Proof) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("<daemon proof>"))
}

// NewPingHandler returns a standard ping handler that proves possession of
// the proof key when challenged. Unchallenged requests retain the ordinary
// PingInfo readiness response.
func (p *Proof) NewPingHandler(rec RuntimeRecord) (http.Handler, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if _, err := proofIdentityForRecord(rec); err != nil {
		return nil, err
	}
	ping := NewPingHandler(PingHandlerOptions{
		Service: rec.Service,
		Version: rec.Version,
		PID:     rec.PID,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenges := r.Header.Values(proofChallengeHeader)
		if r.Method == http.MethodGet && len(challenges) > 0 &&
			!p.writePingProof(w, challenges, rec) {
			return
		}
		ping.ServeHTTP(w, r)
	}), nil
}

func (p *Proof) writePingProof(w http.ResponseWriter, challenges []string, rec RuntimeRecord) bool {
	if len(challenges) != 1 {
		http.Error(w, "invalid daemon proof challenge", http.StatusBadRequest)
		return false
	}
	nonce, err := decodeProofValue(challenges[0], proofNonceSize)
	if err != nil {
		http.Error(w, "invalid daemon proof challenge", http.StatusBadRequest)
		return false
	}
	proof, err := proofMAC(p.key, nonce, rec)
	if err != nil {
		http.Error(w, "daemon proof unavailable", http.StatusInternalServerError)
		return false
	}
	w.Header().Set(proofResponseHeader, proofEncoding.EncodeToString(proof))
	return true
}

// Probe checks that the endpoint in rec answers its ping endpoint and proves
// possession of the proof key for the runtime identity before returning.
func (p *Proof) Probe(ctx context.Context, rec RuntimeRecord, opts ProbeOptions) (PingInfo, error) {
	if err := p.validate(); err != nil {
		return PingInfo{}, err
	}
	ep := rec.Endpoint()
	if ep.Address == "" {
		return PingInfo{}, errors.New("empty daemon endpoint address")
	}
	client := ep.HTTPClient(HTTPClientOptions{
		Timeout:           opts.timeout(),
		DisableKeepAlives: true,
	})
	return p.probeHTTP(ctx, client, ep.BaseURL(), rec, opts)
}

func (p *Proof) probeHTTP(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	rec RuntimeRecord,
	opts ProbeOptions,
) (PingInfo, error) {
	if err := p.validate(); err != nil {
		return PingInfo{}, err
	}
	if _, err := proofIdentityForRecord(rec); err != nil {
		return PingInfo{}, err
	}
	nonce := make([]byte, proofNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return PingInfo{}, fmt.Errorf("generate daemon proof challenge: %w", err)
	}
	challenge := proofEncoding.EncodeToString(nonce)
	info, headers, err := probeHTTP(ctx, client, baseURL, opts, challenge)
	if err != nil {
		return PingInfo{}, err
	}
	proofs := headers.Values(proofResponseHeader)
	if len(proofs) != 1 {
		return PingInfo{}, errors.New("daemon proof response is missing or repeated")
	}
	proof, err := decodeProofValue(proofs[0], sha256.Size)
	if err != nil {
		return PingInfo{}, errors.New("daemon proof response is malformed")
	}
	want, err := proofMAC(p.key, nonce, rec)
	if err != nil {
		return PingInfo{}, err
	}
	if !hmac.Equal(proof, want) {
		return PingInfo{}, errors.New("daemon proof mismatch")
	}
	if info.Service != rec.Service || info.Version != rec.Version || info.PID != rec.PID {
		return PingInfo{}, errors.New("daemon proof identity does not match ping response")
	}
	return info, nil
}

func (p *Proof) validate() error {
	if p == nil || len(p.key) == 0 {
		return errors.New("daemon proof is not initialized")
	}
	return nil
}

type proofIdentity struct {
	service string
	version string
	pid     string
	network string
	address string
}

func proofIdentityForRecord(rec RuntimeRecord) (proofIdentity, error) {
	if rec.Service == "" {
		return proofIdentity{}, errors.New("daemon proof service is empty")
	}
	if rec.PID <= 0 {
		return proofIdentity{}, errors.New("daemon proof PID must be positive")
	}
	ep := rec.Endpoint()
	if ep.Network != NetworkTCP && ep.Network != NetworkUnix {
		return proofIdentity{}, fmt.Errorf("unsupported daemon proof network %q", ep.Network)
	}
	if ep.Address == "" {
		return proofIdentity{}, errors.New("daemon proof address is empty")
	}
	return proofIdentity{
		service: rec.Service,
		version: rec.Version,
		pid:     strconv.Itoa(rec.PID),
		network: ep.Network,
		address: ep.Address,
	}, nil
}

func proofMAC(key, nonce []byte, rec RuntimeRecord) ([]byte, error) {
	if len(key) == 0 {
		return nil, errors.New("daemon proof key is empty")
	}
	if len(nonce) != proofNonceSize {
		return nil, errors.New("daemon proof nonce has invalid length")
	}
	identity, err := proofIdentityForRecord(rec)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	writeProofField(mac, []byte(proofDomain))
	writeProofField(mac, nonce)
	writeProofField(mac, []byte(identity.service))
	writeProofField(mac, []byte(identity.version))
	writeProofField(mac, []byte(identity.pid))
	writeProofField(mac, []byte(identity.network))
	writeProofField(mac, []byte(identity.address))
	return mac.Sum(nil), nil
}

func writeProofField(mac hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = mac.Write(size[:])
	_, _ = mac.Write(value)
}

func decodeProofValue(value string, size int) ([]byte, error) {
	decoded, err := proofEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return nil, errors.New("invalid daemon proof value")
	}
	return decoded, nil
}
