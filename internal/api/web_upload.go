package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const (
	webUploadSocketPath  = "/api/daemon/web-upload"
	webUploadProofDomain = "docbank-web-upload-v1\x00"
	webUploadChunkBytes  = 1 << 20
	webUploadAuthTimeout = 10 * time.Second
	webUploadInactivity  = 30 * time.Second
)

var (
	errWebUploadCanceled = errors.New("browser upload canceled")
	errWebUploadProtocol = errors.New("invalid browser upload protocol")
)

type webUploadMessage struct {
	Type         string         `json:"type"`
	Token        string         `json:"token,omitempty"`
	Nonce        string         `json:"nonce,omitempty"`
	Proof        string         `json:"proof,omitempty"`
	RequestID    string         `json:"request_id,omitempty"`
	ParentID     int64          `json:"parent_id,omitempty"`
	Name         string         `json:"name,omitempty"`
	MIMEType     string         `json:"mime_type,omitempty"`
	ExpectedHash string         `json:"expected_hash,omitempty"`
	ExpectedSize int64          `json:"expected_size,omitempty"`
	Receipt      *UploadReceipt `json:"receipt,omitempty"`
	Status       int            `json:"status,omitempty"`
	Code         string         `json:"code,omitempty"`
	Detail       string         `json:"detail,omitempty"`
}

type webUploadRequest struct {
	requestID    string
	parentID     int64
	name         string
	mimeType     string
	expectedHash string
	expectedSize int64
}

func registerWebUpload(
	mux *http.ServeMux,
	enabled bool,
	webURL string,
	d Deps,
	g *gate,
	sessions *webSessionRegistry,
) {
	if !enabled || webURL == "" {
		return
	}
	origin := strings.TrimSuffix(webURL, "/")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		panic("api: invalid browser origin for verified upload")
	}
	mux.HandleFunc("GET "+webUploadSocketPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin {
			http.Error(w, "browser upload origin rejected", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{parsed.Host},
		})
		if err != nil {
			return // Accept writes its own handshake error.
		}
		conn.SetReadLimit(webUploadChunkBytes + 4096)
		if !sessions.trackUpload(conn) {
			_ = conn.Close(websocket.StatusGoingAway, "daemon is shutting down")
			return
		}
		defer sessions.releaseTrackedUpload(conn)
		handleWebUploadConnection(r.Context(), conn, d, g, sessions)
	})
}

func handleWebUploadConnection(
	ctx context.Context,
	conn *websocket.Conn,
	d Deps,
	g *gate,
	sessions *webSessionRegistry,
) {
	defer func() { _ = conn.CloseNow() }()

	var auth webUploadMessage
	authCtx, cancelAuth := context.WithTimeout(ctx, webUploadAuthTimeout)
	err := wsjson.Read(authCtx, conn, &auth)
	cancelAuth()
	if err != nil {
		return
	}
	nonce, err := base64.RawURLEncoding.DecodeString(auth.Nonce)
	if auth.Type != "authenticate" || len(nonce) != sha256.Size || err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid upload authentication")
		return
	}
	secret, ok := sessions.uploadSecret(auth.Token)
	if !ok || !sessions.bindUpload(auth.Token, conn) {
		_ = conn.Close(websocket.StatusPolicyViolation, "upload session unavailable")
		return
	}
	defer sessions.releaseUpload(auth.Token, conn)
	if err := wsjson.Write(ctx, conn, webUploadMessage{
		Type: "authenticated", Proof: webUploadProof(secret, auth.Token, auth.Nonce),
	}); err != nil {
		return
	}

	for {
		var begin webUploadMessage
		if err := wsjson.Read(ctx, conn, &begin); err != nil {
			return
		}
		request, problem := validateWebUploadBegin(begin)
		if problem != nil {
			if writeWebUploadProblem(ctx, conn, begin.RequestID, problem) != nil {
				return
			}
			continue
		}

		reader := &webUploadReader{
			ctx: ctx, conn: conn, requestID: request.requestID,
			inactivity: webUploadInactivity,
		}
		var result ingest.UploadResult
		ready := false
		uploadErr := g.mutate(func() error {
			if problem := validateWebUploadDestination(ctx, d, request.parentID); problem != nil {
				return problem
			}
			if err := wsjson.Write(ctx, conn, webUploadMessage{
				Type: "ready", RequestID: request.requestID,
			}); err != nil {
				return fmt.Errorf("writing browser upload readiness: %w", err)
			}
			ready = true
			var err error
			result, err = executeWebUpload(ctx, d, request, reader)
			return err
		})
		reader.close()
		if uploadErr != nil {
			problem := uploadError(uploadErr)
			if errors.Is(uploadErr, errWebUploadCanceled) {
				problem = NewError(499, "canceled", "browser upload canceled before authority")
			}
			if writeWebUploadProblem(ctx, conn, request.requestID, problem) != nil {
				return
			}
			if ready && !reader.ended {
				_ = conn.Close(websocket.StatusPolicyViolation,
					"upload stream ended before its terminal marker")
				return
			}
			continue
		}
		status := "skipped"
		if result.Added {
			status = "added"
		}
		receipt := &UploadReceipt{
			Status: status, Node: fromStoreNode(result.Node),
			ComputedHash: result.ComputedHash, ComputedSize: result.ComputedSize,
		}
		if err := wsjson.Write(ctx, conn, webUploadMessage{
			Type: "receipt", RequestID: request.requestID, Receipt: receipt,
		}); err != nil {
			return
		}
	}
}

func validateWebUploadBegin(begin webUploadMessage) (webUploadRequest, *Error) {
	if begin.Type != "begin" || begin.RequestID == "" || len(begin.RequestID) > 128 {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"upload begin requires a bounded request identity")
	}
	name, err := store.NormalizeName(begin.Name)
	if err != nil {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "invalid_name", err.Error())
	}
	parsedHash, err := packstore.ParseHash(begin.ExpectedHash)
	if err != nil || parsedHash.String() != begin.ExpectedHash {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"expected_hash must be canonical lowercase SHA-256")
	}
	if begin.ExpectedSize < 0 || begin.ExpectedSize > blob.MaxIngestBytes {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("expected_size must be between 0 and %d", blob.MaxIngestBytes))
	}
	mimeType, err := uploadMediaType(begin.MIMEType)
	if err != nil {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	return webUploadRequest{
		requestID: begin.RequestID, parentID: begin.ParentID, name: name,
		mimeType: mimeType, expectedHash: begin.ExpectedHash, expectedSize: begin.ExpectedSize,
	}, nil
}

func validateWebUploadDestination(ctx context.Context, d Deps, parentID int64) *Error {
	parent, err := d.Store.NodeByID(ctx, parentID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && parent.TrashedAt != nil):
		return NewError(http.StatusNotFound, "not_found",
			"upload destination does not exist")
	case err != nil:
		if problem, ok := errors.AsType[*Error](FromStoreError(err)); ok {
			return problem
		}
		return NewError(http.StatusInternalServerError, "internal",
			"could not inspect the upload destination")
	case !parent.IsDir():
		return NewError(http.StatusConflict, "not_dir",
			"upload destination is not a directory")
	}
	return nil
}

func executeWebUpload(
	ctx context.Context,
	d Deps,
	request webUploadRequest,
	reader *webUploadReader,
) (result ingest.UploadResult, retErr error) {
	retErr = d.Blobs.WithMutation(ctx, func() error {
		limited := &io.LimitedReader{R: reader, N: request.expectedSize + 1}
		ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
		prepared, err := ing.PrepareUpload(
			ctx, request.parentID, request.name, request.mimeType, limited,
			request.expectedHash, request.expectedSize,
		)
		if err != nil {
			return err
		}
		result, err = prepared.Commit(ctx)
		return err
	})
	return result, retErr
}

type webUploadReader struct {
	ctx        context.Context
	conn       *websocket.Conn
	requestID  string
	current    io.Reader
	cancel     context.CancelFunc
	inactivity time.Duration
	ended      bool
}

func (r *webUploadReader) Read(p []byte) (int, error) {
	for {
		if r.current != nil {
			n, err := r.current.Read(p)
			if err != nil {
				r.current = nil
				r.cancel()
				r.cancel = nil
				if errors.Is(err, io.EOF) && n > 0 {
					return n, nil
				}
				if errors.Is(err, io.EOF) {
					continue
				}
			}
			return n, err
		}
		messageType, next, cancel, err := r.nextFrame()
		if err != nil {
			return 0, err
		}
		switch messageType {
		case websocket.MessageBinary:
			r.current = next
			r.cancel = cancel
		case websocket.MessageText:
			var terminal webUploadMessage
			decoder := json.NewDecoder(io.LimitReader(next, 4096))
			err := decoder.Decode(&terminal)
			cancel()
			if err != nil || terminal.RequestID != r.requestID {
				return 0, errWebUploadProtocol
			}
			r.ended = true
			switch terminal.Type {
			case "end":
				return 0, io.EOF
			case "cancel":
				return 0, errWebUploadCanceled
			default:
				return 0, errWebUploadProtocol
			}
		default:
			cancel()
			return 0, errWebUploadProtocol
		}
	}
}

func (r *webUploadReader) nextFrame() (
	websocket.MessageType, io.Reader, context.CancelFunc, error,
) {
	readCtx, cancel := context.WithTimeout(r.ctx, r.inactivity)
	messageType, next, err := r.conn.Reader(readCtx)
	if err != nil {
		cancel()
		return 0, nil, nil, fmt.Errorf("waiting for browser upload frame: %w", err)
	}
	return messageType, next, cancel, nil
}

func (r *webUploadReader) close() {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.current = nil
}

func writeWebUploadProblem(
	ctx context.Context,
	conn *websocket.Conn,
	requestID string,
	problem *Error,
) error {
	if err := wsjson.Write(ctx, conn, webUploadMessage{
		Type: "error", RequestID: requestID, Status: problem.Status,
		Code: problem.Code, Detail: problem.Detail,
	}); err != nil {
		return fmt.Errorf("writing browser upload problem: %w", err)
	}
	return nil
}

func webUploadProof(secret [sha256.Size]byte, token, nonce string) string {
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write([]byte(webUploadProofDomain))
	_, _ = mac.Write([]byte(token))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
