package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const (
	webUploadSocketPath   = "/api/daemon/web-upload"
	webUploadProofDomain  = "docbank-web-upload-v1\x00"
	webUploadChunkBytes   = 1 << 20
	webPhotoMaxFiles      = 32768
	webPhotoPlanBytes     = 8 << 20
	webPhotoGroupBytes    = 4096 + 67*webPhotoMaxFiles
	webUploadControlBytes = 4096
	webUploadAuthTimeout  = 10 * time.Second
	webUploadInactivity   = 30 * time.Second
)

var (
	errWebUploadCanceled = errors.New("browser upload canceled")
	errWebUploadProtocol = errors.New("invalid browser upload protocol")
	errWebUploadTooBig   = errors.New("upload control message exceeds its limit")
)

type webPhotoFile struct {
	RelativePath string `json:"relative_path"`
	ExpectedHash string `json:"expected_hash"`
	Size         int64  `json:"size"`
	// ModifiedTime is the source file's last-modified time as JavaScript's
	// File.lastModified: milliseconds since the Unix epoch, a pointer so a
	// genuine zero or negative value (a file last modified at or before the
	// epoch, which browsers do report) stays distinct from the field being
	// absent altogether.
	ModifiedTime *int64 `json:"modified_time,omitzero"`
}

// originalMtime canonicalizes a browser-supplied modified_time into the
// canonical UTC RFC3339Nano string store.PhotoImportMember.OriginalMtime
// requires, the same format the CLI importer derives from the filesystem's
// own mtime. A nil value means the browser didn't report one, so provenance
// records no original modified time for it; zero and negative values are
// genuine timestamps and are preserved.
func originalMtime(modifiedTimeMillis *int64) string {
	if modifiedTimeMillis == nil {
		return ""
	}
	return time.UnixMilli(*modifiedTimeMillis).UTC().Format(time.RFC3339Nano)
}

type webPhotoReceipt struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitzero"`
	AssetID string `json:"asset_id"`
}

type webUploadMessage struct {
	SourceID     *uuid.UUID          `json:"source_id,omitzero"`
	ImportID     string              `json:"import_id,omitzero"`
	Files        []webPhotoFile      `json:"files,omitzero"`
	Groups       [][]int             `json:"groups,omitzero"`
	Unsupported  []int               `json:"unsupported,omitzero"`
	SkipReasons  map[int]string      `json:"skip_reasons,omitzero"`
	Destination  string              `json:"destination,omitzero"`
	Group        int                 `json:"group,omitzero"`
	Hashes       []string            `json:"hashes,omitzero"`
	Indexes      []int               `json:"indexes,omitzero"`
	PhotoReceipt *webPhotoReceipt    `json:"photo_group_receipt,omitempty"`
	ContainerID  string              `json:"container_id,omitzero"`
	ChunkIndex   int                 `json:"chunk_index,omitzero"`
	ChunkReceipt *store.MailboxChunk `json:"chunk_receipt,omitempty"`
	Type         string              `json:"type"`
	Token        string              `json:"token,omitzero"`
	Nonce        string              `json:"nonce,omitzero"`
	Proof        string              `json:"proof,omitzero"`
	RequestID    string              `json:"request_id,omitzero"`
	ParentID     int64               `json:"parent_id,omitzero"`
	Name         string              `json:"name,omitzero"`
	MIMEType     string              `json:"mime_type,omitzero"`
	ExpectedHash string              `json:"expected_hash,omitzero"`
	ExpectedSize int64               `json:"expected_size,omitzero"`
	Receipt      *UploadReceipt      `json:"receipt,omitempty"`
	Status       int                 `json:"status,omitzero"`
	Code         string              `json:"code,omitzero"`
	Detail       string              `json:"detail,omitzero"`
}

type webUploadRequest struct {
	requestID    string
	containerID  string
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
		loginOrigin := sessions.login != nil && webSameOrigin(r, sessions.login, true)
		if r.Header.Get("Origin") != origin && !loginOrigin {
			http.Error(w, "browser upload origin rejected", http.StatusForbidden)
			return
		}
		allowedHost := parsed.Host
		if loginOrigin {
			allowedHost = r.Host
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{allowedHost},
		})
		if err != nil {
			return // Accept writes its own handshake error.
		}
		conn.SetReadLimit(4096)
		if !sessions.trackUpload(conn) {
			_ = conn.Close(websocket.StatusGoingAway, "daemon is shutting down")
			return
		}
		defer sessions.releaseTrackedUpload(conn)
		handleWebUploadConnection(r.Context(), conn, d, g, sessions, r)
	})
}

// webUploadControlLimit returns the largest control message the upload
// socket accepts for a message type. Photo plans and groups list every file,
// so they get larger limits than the fixed-size upload messages.
func webUploadControlLimit(kind string) int {
	switch kind {
	case "plan_photo_import":
		return webPhotoPlanBytes
	case "begin_photo_group":
		return webPhotoGroupBytes
	default:
		return webUploadControlBytes
	}
}

// readWebUploadControl reads one text control message and enforces the
// limit for its declared type. It decodes the type with the same decoder that
// later decodes the whole message, so the limit always matches the handled
// type.
func readWebUploadControl(ctx context.Context, conn *websocket.Conn) (webUploadMessage, error) {
	conn.SetReadLimit(webPhotoPlanBytes)
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return webUploadMessage{}, fmt.Errorf("reading web upload control message: %w", err)
	}
	if kind != websocket.MessageText {
		return webUploadMessage{}, errWebUploadProtocol
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return webUploadMessage{}, fmt.Errorf("decoding web upload control type: %w", err)
	}
	if len(data) > webUploadControlLimit(header.Type) {
		return webUploadMessage{}, errWebUploadTooBig
	}
	var message webUploadMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return webUploadMessage{}, fmt.Errorf("decoding web upload control message: %w", err)
	}
	return message, nil
}

func handleWebUploadConnection(
	ctx context.Context,
	conn *websocket.Conn,
	d Deps,
	g *gate,
	sessions *webSessionRegistry,
	request *http.Request,
) {
	defer func() { _ = conn.CloseNow() }()

	var auth webUploadMessage
	authCtx, cancelAuth := context.WithTimeout(ctx, webUploadAuthTimeout)
	err := wsjson.Read(authCtx, conn, &auth)
	cancelAuth()
	if err != nil {
		if errors.Is(err, websocket.ErrMessageTooBig) {
			_ = conn.Close(websocket.StatusMessageTooBig, "authentication message exceeds its limit")
		}
		return
	}
	nonce, err := base64.RawURLEncoding.DecodeString(auth.Nonce)
	if auth.Type != "authenticate" || len(nonce) != sha256.Size || err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid upload authentication")
		return
	}
	if !sessions.authorizeBrowser(request, auth.Token) {
		_ = conn.Close(websocket.StatusPolicyViolation, "browser cookie rejected")
		return
	}
	secret, ok := sessions.uploadSecret(auth.Token)
	if !ok || !sessions.bindUpload(auth.Token, conn) {
		_ = conn.Close(websocket.StatusPolicyViolation, "upload session unavailable")
		return
	}
	owner, sessionCtx, ok := sessions.authenticate(auth.Token)
	if !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, "upload session unavailable")
		return
	}
	defer sessions.releaseUpload(auth.Token, conn)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(sessionCtx, cancel)
	defer stop()
	defer cancel()
	if err := wsjson.Write(ctx, conn, webUploadMessage{
		Type: "authenticated", Proof: webUploadProof(secret, auth.Token, auth.Nonce),
	}); err != nil {
		return
	}

	var photoImport *webPhotoImport
	for {
		begin, err := readWebUploadControl(ctx, conn)
		if err != nil {
			if errors.Is(err, errWebUploadTooBig) {
				_ = conn.Close(websocket.StatusMessageTooBig,
					"upload control message exceeds its limit")
			}
			return
		}
		if begin.Type == "plan_photo_import" {
			var ok bool
			photoImport, ok = handleWebPhotoPlan(ctx, conn, d, begin)
			if !ok {
				return
			}
			continue
		}
		if begin.Type == "begin_photo_group" {
			if !handleWebPhotoGroup(ctx, conn, d, g, begin, photoImport) {
				return
			}
			continue
		}
		if begin.Type == "begin_mailbox_chunk" {
			if !handleWebMailboxChunk(ctx, conn, d, g, begin) {
				return
			}
			continue
		}
		request, problem := validateWebUploadBegin(begin)
		if problem != nil {
			if writeWebUploadProblem(ctx, conn, begin.RequestID, problem) != nil {
				return
			}
			continue
		}
		if request.containerID != "" {
			if !handleWebPackageContainer(ctx, conn, d, g, owner, request) {
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

// isCanonicalHash reports whether hash is packstore's canonical lowercase
// SHA-256 encoding of itself, the acceptance rule every expected_hash on the
// browser upload socket shares, whether for one file or a photo group.
func isCanonicalHash(hash string) bool {
	parsed, err := packstore.ParseHash(hash)
	return err == nil && parsed.String() == hash
}

func validateWebUploadBegin(begin webUploadMessage) (webUploadRequest, *Error) {
	if begin.Type != "begin" || begin.RequestID == "" || len(begin.RequestID) > 128 {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"upload begin requires a bounded request identity")
	}
	if !isCanonicalHash(begin.ExpectedHash) {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"expected_hash must be canonical lowercase SHA-256")
	}
	if begin.ContainerID != "" {
		if len(begin.ContainerID) > 128 || begin.ExpectedSize < 1 || begin.ExpectedSize > store.MailboxContainerBytes || begin.ParentID != 0 || begin.Name != "" || begin.MIMEType != "" {
			return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "validation", "invalid package container upload declaration")
		}
		return webUploadRequest{requestID: begin.RequestID, containerID: begin.ContainerID,
			expectedHash: begin.ExpectedHash, expectedSize: begin.ExpectedSize}, nil
	}
	name, err := store.NormalizeName(begin.Name)
	if err != nil {
		return webUploadRequest{}, NewError(http.StatusUnprocessableEntity, "invalid_name", err.Error())
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

func handleWebPackageContainer(ctx context.Context, conn *websocket.Conn, d Deps, g *gate, owner string, request webUploadRequest) bool {
	reader := &webUploadReader{ctx: ctx, conn: conn, requestID: request.requestID, inactivity: webUploadInactivity}
	ready := false
	err := g.mutate(func() (err error) {
		c, err := d.Store.MailboxContainer(ctx, owner, request.containerID)
		if err != nil {
			return err
		}
		if c.Format != "zip" || c.State != "uploading" || c.SHA256 != request.expectedHash || c.Size != request.expectedSize {
			return store.ErrMailboxConflict
		}
		defer func() {
			if err == nil {
				return
			}
			// The whole-container socket transfer is not resumable. Keep cleanup
			// inside its mutation admission, even if the connection was canceled.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			abortErr := d.Store.AbortMailboxContainer(cleanup, owner, request.containerID)
			if abortErr != nil && !errors.Is(abortErr, store.ErrNotFound) && !errors.Is(abortErr, store.ErrMailboxConflict) {
				d.Logger.Error("abort incomplete browser package upload", "error", abortErr)
			}
		}()
		if err := wsjson.Write(ctx, conn, webUploadMessage{Type: "ready", RequestID: request.requestID}); err != nil {
			return fmt.Errorf("ready package upload: %w", err)
		}
		ready = true
		if err := executeWebPackageContainer(ctx, d, owner, request, reader); err != nil {
			return err
		}
		return wsjson.Write(ctx, conn, webUploadMessage{Type: "package_container_receipt", RequestID: request.requestID, ContainerID: request.containerID})
	})
	if err != nil {
		problem := mailboxError(err)
		if errors.Is(err, errWebUploadProtocol) {
			problem = NewError(http.StatusUnprocessableEntity, "validation", "invalid browser upload frame")
		}
		if errors.Is(err, errWebUploadCanceled) {
			problem = NewError(499, "canceled", "package upload canceled")
		}
		if writeWebUploadProblem(ctx, conn, request.requestID, problem) != nil {
			return false
		}
		return !ready || reader.ended
	}
	return true
}

func executeWebPackageContainer(ctx context.Context, d Deps, owner string, request webUploadRequest, reader *webUploadReader) error {
	service := mailboxService(d)
	fullHash := sha256.New()
	remaining := request.expectedSize
	for index := 0; remaining > 0; index++ {
		size := min(remaining, store.MailboxChunkBytes)
		chunk := make([]byte, int(size))
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return errors.Join(store.ErrMailboxConflict, err)
		}
		_, _ = fullHash.Write(chunk)
		chunkHash := sha256.Sum256(chunk)
		if err := service.UploadChunk(ctx, owner, request.containerID, index, hex.EncodeToString(chunkHash[:]), size, bytes.NewReader(chunk)); err != nil {
			return err
		}
		remaining -= size
	}
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if !reader.ended || n != 0 || !errors.Is(err, io.EOF) || hex.EncodeToString(fullHash.Sum(nil)) != request.expectedHash {
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return store.ErrMailboxConflict
	}
	return nil
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
	inactivity time.Duration
	ended      bool
}

func (r *webUploadReader) Read(p []byte) (int, error) {
	for {
		if r.current != nil {
			n, err := r.current.Read(p)
			if err != nil {
				r.current = nil
				if errors.Is(err, io.EOF) && n > 0 {
					return n, nil
				}
				if errors.Is(err, io.EOF) {
					continue
				}
			}
			return n, err
		}
		readCtx, cancel := context.WithTimeout(r.ctx, r.inactivity)
		// Finish the bounded frame before consumer work can outlive its read deadline.
		r.conn.SetReadLimit(webUploadChunkBytes + 4096)
		messageType, data, err := r.conn.Read(readCtx)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("reading browser upload frame: %w", err)
		}
		switch messageType {
		case websocket.MessageBinary:
			if len(data) > webUploadChunkBytes {
				return 0, errWebUploadProtocol
			}
			r.current = bytes.NewReader(data)
		case websocket.MessageText:
			if len(data) > 4096 {
				return 0, errWebUploadProtocol
			}
			var terminal webUploadMessage
			err := json.Unmarshal(data, &terminal)
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
			return 0, errWebUploadProtocol
		}
	}
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

func planWebPhotos(
	requestID string, files []webPhotoFile,
) ([][]int, []int, map[int]string, *Error) {
	invalid := func() ([][]int, []int, map[int]string, *Error) {
		return nil, nil, nil, NewError(http.StatusUnprocessableEntity, "validation",
			"invalid photo import declaration")
	}
	if requestID == "" || len(requestID) > 128 || len(files) == 0 || len(files) > webPhotoMaxFiles {
		return invalid()
	}
	indexes := make(map[string]int, len(files))
	candidates := make([]ingest.PhotoImportCandidate, 0, len(files))
	unsupported := []int{}
	skipReasons := make(map[int]string)
	for index, file := range files {
		name := file.RelativePath
		reason := ""
		if file.Size < 0 || file.Size > blob.MaxIngestBytes {
			reason = "Invalid size"
		}
		if strings.Contains(name, "\\") {
			reason = "Invalid name"
		}
		for part := range strings.SplitSeq(name, "/") {
			if _, err := store.NormalizeName(part); err != nil {
				reason = "Invalid name"
			}
		}
		if _, exists := indexes[name]; exists {
			reason = "Duplicate path"
		}
		if reason != "" {
			unsupported = append(unsupported, index)
			skipReasons[index] = reason
			continue
		}
		source := store.ClassifyPhotoSource(name)
		if source.Kind == store.PhotoSourceUnsupported {
			unsupported = append(unsupported, index)
			skipReasons[index] = "Unsupported type"
			continue
		}
		if file.ExpectedHash != "" && !isCanonicalHash(file.ExpectedHash) {
			return invalid()
		}
		indexes[name] = index
		candidates = append(candidates, ingest.PhotoImportCandidate{Path: name, Kind: source.Kind})
	}
	groups := [][]int{}
	for _, group := range ingest.GroupPhotoCandidates(candidates) {
		members := make([]int, 0, len(group.Members))
		for _, member := range group.Members {
			members = append(members, indexes[member.Path])
		}
		groups = append(groups, members)
	}
	return groups, unsupported, skipReasons, nil
}

type webPhotoImport struct {
	id          string
	files       []webPhotoFile
	paths       []string
	groups      [][]int
	run         store.IngestRun
	destination store.IngestDirectoryPlan
}

func (p *webPhotoImport) sourceDescription() string {
	folder, _, nested := strings.Cut(p.files[0].RelativePath, "/")
	if nested {
		for _, file := range p.files {
			if !strings.HasPrefix(file.RelativePath, folder+"/") {
				return "browser: Choose files"
			}
		}
		return "browser: " + folder
	}
	return "browser: Choose files"
}

// unhashedBrowserPhotos lists the planned files that declared no hash but
// match the size of an eligible earlier browser photo. Only exact bytes can
// continue a folder, so these are the only files the browser must hash before
// the plan; the rest hash with their group at upload.
func unhashedBrowserPhotos(
	ctx context.Context, s *store.Store, files []webPhotoFile, groups [][]int,
) ([]int, *Error) {
	var sizes []int64
	for _, group := range groups {
		for _, index := range group {
			if files[index].ExpectedHash == "" {
				sizes = append(sizes, files[index].Size)
			}
		}
	}
	if len(sizes) == 0 {
		return nil, nil
	}
	candidates, err := s.BrowserPhotoSizeCandidates(ctx, sizes)
	if err != nil {
		return nil, uploadError(err)
	}
	var needed []int
	for _, group := range groups {
		for _, index := range group {
			if files[index].ExpectedHash == "" && candidates[files[index].Size] {
				needed = append(needed, index)
			}
		}
	}
	slices.Sort(needed)
	return needed, nil
}

// handleWebPhotoPlan answers one plan_photo_import message. It returns the
// new import, or nil when the plan was rejected, and false when the socket
// must close.
func handleWebPhotoPlan(
	ctx context.Context, conn *websocket.Conn, d Deps, begin webUploadMessage,
) (*webPhotoImport, bool) {
	problem := validatePhotoImportDestination(begin.Destination)
	if begin.SourceID != nil && *begin.SourceID == uuid.Nil() {
		problem = NewError(http.StatusUnprocessableEntity, "validation", "source_id must be nonzero")
	}
	var destination store.IngestDirectoryPlan
	if problem == nil {
		var err error
		destination, err = d.Store.PreparePhotoImportDestination(ctx, begin.Destination)
		if err != nil {
			problem, _ = errors.AsType[*Error](FromStoreError(err))
		}
	}
	var groups [][]int
	var unsupported []int
	var skipReasons map[int]string
	if problem == nil {
		groups, unsupported, skipReasons, problem = planWebPhotos(begin.RequestID, begin.Files)
	}
	var needed []int
	if problem == nil {
		needed, problem = unhashedBrowserPhotos(ctx, d.Store, begin.Files, groups)
	}
	if problem != nil {
		return nil, writeWebUploadProblem(ctx, conn, begin.RequestID, problem) == nil
	}
	if len(needed) > 0 {
		return nil, wsjson.Write(ctx, conn, webUploadMessage{
			Type: "photo_import_hashes_needed", RequestID: begin.RequestID, Indexes: needed,
		}) == nil
	}
	sourceID := uuid.New()
	if begin.SourceID != nil {
		sourceID = *begin.SourceID
	}
	paths := make([]string, len(begin.Files))
	for i, file := range begin.Files {
		paths[i] = "browser:" + sourceID.String() + "/" + file.RelativePath
	}
	var members []store.PhotoImportMember
	var indexes []int
	for _, group := range groups {
		for _, index := range group {
			members = append(members, store.PhotoImportMember{BlobHash: begin.Files[index].ExpectedHash, OriginalPath: paths[index]})
			indexes = append(indexes, index)
		}
	}
	resolved, resolveErr := d.Store.ResolveBrowserPhotoPaths(ctx, members)
	if resolveErr != nil {
		return nil, writeWebUploadProblem(ctx, conn, begin.RequestID, uploadError(resolveErr)) == nil
	}
	for i, index := range indexes {
		paths[index] = resolved[i]
	}
	photoImport := &webPhotoImport{
		paths: paths,
		id:    rand.Text(), files: begin.Files, groups: groups, destination: destination,
	}
	err := wsjson.Write(ctx, conn, webUploadMessage{
		Type: "photo_import_plan", RequestID: begin.RequestID, ImportID: photoImport.id,
		Groups: groups, Unsupported: unsupported, SkipReasons: skipReasons,
	})
	return photoImport, err == nil
}

// groupFiles checks that begin names a group of this import plan, with one
// canonical hash per member that matches any hash declared in the plan, and
// returns the group's files. A member declared without a hash is pinned to the
// hash of its first attempt, so a retry must upload the same bytes.
func (p *webPhotoImport) groupFiles(begin webUploadMessage) ([]webPhotoFile, *Error) {
	invalid := func(detail string) ([]webPhotoFile, *Error) {
		return nil, NewError(http.StatusUnprocessableEntity, "validation", detail)
	}
	if p == nil || begin.ImportID != p.id || begin.Group < 0 || begin.Group >= len(p.groups) ||
		begin.RequestID == "" || len(begin.RequestID) > 128 {
		return invalid("photo group must belong to the current import plan")
	}
	group := p.groups[begin.Group]
	if len(begin.Hashes) != len(group) {
		return invalid("Photo files must match the planned group. Choose the folder again.")
	}
	files := make([]webPhotoFile, len(group))
	for i, index := range group {
		declared := p.files[index].ExpectedHash
		if !isCanonicalHash(begin.Hashes[i]) || declared != "" && begin.Hashes[i] != declared {
			return invalid("expected_hash must be canonical lowercase SHA-256 and match the planned content. Choose the folder again.")
		}
		files[i] = p.files[index]
	}
	for i, index := range group {
		p.files[index].ExpectedHash = begin.Hashes[i]
	}
	return files, nil
}

func handleWebPhotoGroup(
	ctx context.Context, conn *websocket.Conn, d Deps, g *gate,
	begin webUploadMessage, photoImport *webPhotoImport,
) bool {
	files, problem := photoImport.groupFiles(begin)
	if problem != nil {
		return writeWebUploadProblem(ctx, conn, begin.RequestID, problem) == nil
	}
	var reader *webUploadReader
	var result store.PhotoImportResult
	err := g.mutate(func() error {
		return d.Blobs.WithMutation(ctx, func() (retErr error) {
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			prepared := make([]*ingest.PreparedUpload, 0, len(files))
			defer func() {
				for _, member := range prepared {
					retErr = errors.Join(retErr, member.Discard())
				}
			}()
			for i, member := range files {
				err := wsjson.Write(ctx, conn, webUploadMessage{
					Type: "ready", RequestID: begin.RequestID,
				})
				if err != nil {
					return fmt.Errorf("ready photo member: %w", err)
				}
				reader = &webUploadReader{
					ctx: ctx, conn: conn, requestID: begin.RequestID,
					inactivity: webUploadInactivity,
				}
				mediaType := store.ClassifyPhotoSource(member.RelativePath).MediaType
				body := &io.LimitedReader{R: reader, N: member.Size + 1}
				upload, err := ing.PrepareUpload(ctx, d.Store.RootID(),
					path.Base(member.RelativePath), mediaType, body, begin.Hashes[i], member.Size)
				if err != nil {
					return err
				}
				prepared = append(prepared, upload)
			}
			paths := make([]string, len(files))
			mtimes := make([]string, len(files))
			for i, member := range files {
				paths[i] = photoImport.paths[photoImport.groups[begin.Group][i]]
				mtimes[i] = originalMtime(member.ModifiedTime)
			}
			var err error
			if photoImport.run.ID() == "" {
				photoImport.run, err = d.Store.BeginIngest(ctx, "photo-import",
					photoImport.sourceDescription())
				if err != nil {
					return err
				}
			}
			result, err = ing.CommitPhotoGroup(ctx, ingest.PhotoGroupCommit{
				Run:         photoImport.run,
				Destination: &photoImport.destination, Paths: paths, Mtimes: mtimes,
			}, prepared)
			prepared = nil
			return err
		})
	})
	if err != nil {
		problem := uploadError(err)
		if errors.Is(err, errWebUploadCanceled) {
			problem = NewError(499, "canceled", "photo group upload canceled")
		}
		if writeWebUploadProblem(ctx, conn, begin.RequestID, problem) != nil {
			return false
		}
		return reader == nil || reader.ended
	}
	status := "skipped"
	if result.Ambiguity != nil {
		status = "ambiguous"
	} else if result.Added {
		status = "added"
	}
	reason := ""
	if result.Ambiguity != nil {
		reason = result.Ambiguity.Reason
	}
	return wsjson.Write(ctx, conn, webUploadMessage{
		Type: "photo_group_receipt", RequestID: begin.RequestID,
		PhotoReceipt: &webPhotoReceipt{Status: status, AssetID: result.Asset.ID, Reason: reason},
	}) == nil
}
