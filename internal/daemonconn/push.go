package daemonconn

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	clientruntime "github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/store"
)

const pushProofTimeout = 30 * time.Second

// errKeyProof marks an endpoint that did not prove it holds the API key.
var errKeyProof = errors.New("daemon endpoint did not prove it holds the API key")

// NewPushConnection connects only to the explicitly selected keyed daemon.
// Every new socket must prove possession of the API key before the key or any
// document bytes cross it, so a listener that captures a forwarded port after
// the tunnel exits receives neither. Redirects and proxies are not followed.
func NewPushConnection(origin, key string) (*Connection, error) {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("push requires an http(s) daemon origin without credentials, path, query, or fragment")
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("push requires a nonempty API key")
	}
	base := parsed.Scheme + "://" + parsed.Host
	prover := keyProver{base: base, key: key}
	transport := &http.Transport{
		Proxy:                 nil,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if parsed.Scheme == "http" {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, fmt.Errorf("dialing daemon: %w", err)
			}
			return prover.prove(ctx, conn)
		}
	} else {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			ServerName: parsed.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12,
		}}
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := tlsDialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, fmt.Errorf("dialing daemon over TLS: %w", err)
			}
			return prover.prove(ctx, conn)
		}
	}
	connection := New(base, key)
	connection.hc = &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return connection, nil
}

type keyProver struct {
	base string
	key  string
}

// prove runs the credential-free key challenge on conn and returns conn only
// when the endpoint answered with a valid proof. The request carries no key.
func (p keyProver) prove(ctx context.Context, conn net.Conn) (net.Conn, error) {
	proofCtx, cancel := context.WithTimeout(ctx, pushProofTimeout)
	defer cancel()
	stop := context.AfterFunc(proofCtx, func() { _ = conn.Close() })
	nonce := make([]byte, daemonauth.NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		stop()
		_ = conn.Close()
		return nil, fmt.Errorf("generating API key challenge: %w", err)
	}
	challenger := New(p.base, "")
	challenger.hc = &http.Client{Transport: proofTransport{conn: conn}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("daemon key challenge must not redirect")
	}}
	proved, err := challenger.answersKeyChallenge(proofCtx, nonce, p.key)
	if !stop() || proofCtx.Err() != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proving daemon key possession: %w", errors.Join(proofCtx.Err(), err))
	}
	if err != nil && challengeMayRecover(err) {
		_ = conn.Close()
		return nil, fmt.Errorf("daemon key challenge: %w", err)
	}
	if err != nil || !proved {
		_ = conn.Close()
		return nil, errors.Join(errKeyProof, err)
	}
	return conn, nil
}

// challengeMayRecover reports whether a challenge failed before the endpoint
// gave a usable answer: an I/O failure on the socket or a temporary HTTP
// status. A fresh socket repeats the whole challenge, so retrying exposes
// nothing. A wrong proof, a malformed response, or any other status means the
// endpoint is not the keyed daemon and stays terminal.
func challengeMayRecover(err error) bool {
	if errors.Is(err, errInvalidProofResponse) {
		return false
	}
	if IsTransportError(err) {
		return true
	}
	status, ok := ResponseStatus(err)
	return ok && (status >= http.StatusInternalServerError ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests)
}

func (c *Connection) answersKeyChallenge(ctx context.Context, nonce []byte, key string) (bool, error) {
	var responseHTTP *http.Response
	_, err := c.apiWithResponse(&responseHTTP).ChallengeAPIKey(clientruntime.WithStreamingResponse(ctx),
		&apiclient.ChallengeAPIKeyRequestOptions{Query: &apiclient.ChallengeAPIKeyQuery{Nonce: hex.EncodeToString(nonce)}})
	if err != nil {
		return false, err
	}
	defer func() { _ = responseHTTP.Body.Close() }()
	var result struct {
		Proof string `json:"proof"`
	}
	if err := json.UnmarshalRead(io.LimitReader(responseHTTP.Body, proofBodyLimit), &result); err != nil {
		return false, nil
	}
	return daemonauth.VerifyKey(key, nonce, result.Proof), nil
}

// IsKeyProofError reports whether a request failed because the endpoint did
// not prove it holds the API key.
func IsKeyProofError(err error) bool { return errors.Is(err, errKeyProof) }

// PushUpload uses a distinct route so incompatible daemons cannot ignore the
// source identity. content.ParentPath and content.Name place only a new node.
// Only a matching independently computed receipt is success.
func (c *Connection) PushUpload(
	ctx context.Context, source store.PushSource, content store.PushContent, body io.Reader,
) (api.PushUploadReceipt, error) {
	var receipt api.PushUploadReceipt
	if err := store.ValidatePushSource(source); err != nil {
		return receipt, err
	}
	if err := store.ValidatePushDestination(content.ParentPath); err != nil {
		return receipt, err
	}
	mimeType, err := validateUploadRequest(content.Name, content.MIMEType, content.Hash, content.Size, body)
	if err != nil {
		return receipt, err
	}
	query := &apiclient.UploadPushFileQuery{
		ParentPath: content.ParentPath, Name: content.Name, PushName: source.Name,
		SourceRef: source.Ref, Duplicates: source.Duplicates,
	}
	if source.ModifiedAt != "" {
		query.ModifiedAt = &source.ModifiedAt
	}
	resp, err := streamUpload(ctx, content.Name, mimeType, body, func(
		ctx context.Context, response **http.Response, editor clientruntime.RequestEditorFn,
	) error {
		_, callErr := c.apiWithResponse(response).UploadPushFile(ctx, &apiclient.UploadPushFileRequestOptions{
			Query:  query,
			Header: &apiclient.UploadPushFileHeaders{XDocbankBlobHash: content.Hash, XDocbankBlobSize: content.Size},
		}, editor)
		return callErr
	})
	if err != nil {
		return receipt, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.UnmarshalRead(resp.Body, &receipt); err != nil {
		return api.PushUploadReceipt{}, &responseDecodeError{err: fmt.Errorf("decoding push upload response: %w", err)}
	}
	switch receipt.Status {
	case "added", "updated", "linked", "skipped", "duplicate_skipped":
	default:
		return api.PushUploadReceipt{}, &responseDecodeError{err: errors.New("push receipt has an unknown outcome")}
	}
	if receipt.Node.ID <= 0 || receipt.Node.Kind != "file" || receipt.Node.Revision <= 0 ||
		receipt.ComputedHash != content.Hash || receipt.ComputedSize != content.Size {
		return api.PushUploadReceipt{}, &responseDecodeError{err: errors.New("push receipt does not confirm the declared file identity")}
	}
	return receipt, nil
}
