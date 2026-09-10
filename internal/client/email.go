package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func (c *Client) EmailMetadata(ctx context.Context, versionID string) (api.EmailMetadata, error) {
	var metadata api.EmailMetadata
	if !validUUIDv4(versionID) {
		return metadata, errors.New("email metadata requires a canonical UUIDv4 version ID")
	}
	req, err := c.emailRequest(ctx, http.MethodGet, emailMetadataPath(versionID), nil)
	if err != nil {
		return metadata, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return metadata, &transportError{err: fmt.Errorf("calling daemon (GET email metadata): %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusAccepted {
		var pending api.EmailPending
		if err := json.UnmarshalRead(resp.Body, &pending); err != nil {
			return metadata, &responseDecodeError{err: fmt.Errorf("decoding pending email metadata: %w", err)}
		}
		if pending.State != "pending" || pending.Version.ID != versionID || validateEmailVersion(pending.Version) != nil {
			return metadata, integrityErrorf("pending email metadata response has inconsistent version identity")
		}
		metadata.Version = pending.Version
		return metadata, store.ErrEmailPending
	}
	if resp.StatusCode != http.StatusOK {
		return metadata, decodeError(resp)
	}
	if err := json.UnmarshalRead(resp.Body, &metadata); err != nil {
		return api.EmailMetadata{}, &responseDecodeError{err: fmt.Errorf("decoding email metadata: %w", err)}
	}
	if err := validateEmailMetadata(metadata, versionID, ""); err != nil {
		return api.EmailMetadata{}, err
	}
	return metadata, nil
}

func (c *Client) EnsureEmailMetadata(ctx context.Context, versionID string) (api.EmailMetadata, error) {
	var metadata api.EmailMetadata
	if !validUUIDv4(versionID) {
		return metadata, errors.New("email ensure requires a canonical UUIDv4 version ID")
	}
	if err := c.do(ctx, http.MethodPost, emailMetadataPath(versionID), nil, struct{}{}, &metadata); err != nil {
		return api.EmailMetadata{}, err
	}
	if err := validateEmailMetadata(metadata, versionID, ""); err != nil {
		return api.EmailMetadata{}, err
	}
	return metadata, nil
}

func (c *Client) EmailMetadataGeneration(
	ctx context.Context, versionID, generationID string,
) (api.EmailMetadata, error) {
	var metadata api.EmailMetadata
	if !validUUIDv4(versionID) || !validSHA256Hex(generationID) {
		return metadata, errors.New("email generation requires a canonical version ID and generation SHA-256")
	}
	path := emailMetadataPath(versionID) + "/generations/" + url.PathEscape(generationID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &metadata); err != nil {
		return api.EmailMetadata{}, err
	}
	if err := validateEmailMetadata(metadata, versionID, generationID); err != nil {
		return api.EmailMetadata{}, err
	}
	return metadata, nil
}

func (c *Client) OpenEmailPart(
	ctx context.Context, versionID, generationID, partPath, role string,
) (io.ReadCloser, api.EmailPartReceipt, error) {
	if err := document.ValidateEmailPartPath(partPath); err != nil || !emailPartRole(role) {
		return nil, api.EmailPartReceipt{}, store.ErrInvalidEmailPart
	}
	metadata, err := c.EmailMetadataGeneration(ctx, versionID, generationID)
	if err != nil {
		return nil, api.EmailPartReceipt{}, err
	}
	part, artifact, err := emailArtifact(metadata.Evidence, partPath, role)
	if err != nil {
		return nil, api.EmailPartReceipt{}, err
	}
	receipt := api.EmailPartReceipt{
		Version: metadata.Version, GenerationID: metadata.GenerationID,
		AttachmentID: metadata.AttachmentID, PartPath: partPath, Role: role,
		BlobSHA256: artifact.SHA256, Size: artifact.Size,
	}
	path := emailMetadataPath(versionID) + "/generations/" + url.PathEscape(generationID) +
		"/parts/" + url.PathEscape(partPath) + "/" + url.PathEscape(role)
	req, err := c.emailRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, api.EmailPartReceipt{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, api.EmailPartReceipt{}, &transportError{err: fmt.Errorf("calling daemon (GET email part): %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, api.EmailPartReceipt{}, decodeError(resp)
	}
	if err := validateEmailPartHeaders(resp, receipt, expectedEmailPartFilename(part)); err != nil {
		_ = resp.Body.Close()
		return nil, api.EmailPartReceipt{}, err
	}
	return &verifiedEmailPartReader{
		body: resp.Body, trailer: resp.Trailer, expectedHash: receipt.BlobSHA256,
		expectedSize: receipt.Size, hash: sha256.New(),
	}, receipt, nil
}

func expectedEmailPartFilename(part document.EmailPartV1) string {
	if part.Filename.SafeName != "" {
		return part.Filename.SafeName
	}
	name, err := document.SafeEmailFilename("", part.Path)
	if err != nil {
		return "email-part"
	}
	return name
}

func emailMetadataPath(versionID string) string {
	return "/api/v1/versions/" + url.PathEscape(versionID) + "/email"
}

func (c *Client) emailRequest(
	ctx context.Context, method, path string, body io.Reader,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	return req, nil
}

func validateEmailMetadata(metadata api.EmailMetadata, versionID, generationID string) error {
	if metadata.Version.ID != versionID || validateEmailVersion(metadata.Version) != nil ||
		!validSHA256Hex(metadata.GenerationID) || !validSHA256Hex(metadata.AttachmentID) ||
		!validSHA256Hex(metadata.RecipeFingerprint) || !validSHA256Hex(metadata.Checksum) ||
		metadata.CreatedAt == "" || metadata.PublishedAt == "" {
		return integrityErrorf("email metadata response has inconsistent identity")
	}
	if generationID != "" && metadata.GenerationID != generationID {
		return integrityErrorf("email metadata returned generation %s, expected %s", metadata.GenerationID, generationID)
	}
	canonical, checksum, err := document.MarshalEmailV1(metadata.Evidence)
	if err != nil || len(canonical) == 0 || checksum != metadata.Checksum {
		return integrityErrorf("email metadata response has invalid canonical evidence")
	}
	recipe, err := document.EmailRecipeFingerprint(metadata.Evidence.Recipe)
	if err != nil || recipe != metadata.RecipeFingerprint {
		return integrityErrorf("email metadata response has inconsistent recipe identity")
	}
	derivedGeneration, err := document.EmailGenerationID(metadata.Evidence, checksum)
	if err != nil || derivedGeneration != metadata.GenerationID {
		return integrityErrorf("email metadata response has inconsistent generation identity")
	}
	derivedAttachment, err := document.EmailAttachmentID(metadata.Version.ID, metadata.GenerationID)
	if err != nil || derivedAttachment != metadata.AttachmentID {
		return integrityErrorf("email metadata response has inconsistent attachment identity")
	}
	if metadata.Evidence.Source.SHA256 != metadata.Version.BlobHash ||
		metadata.Evidence.Source.Size != metadata.Version.Size {
		return integrityErrorf("email metadata response has inconsistent source identity")
	}
	return nil
}

func validateEmailVersion(version api.ContentVersion) error {
	if !validUUIDv4(version.ID) || version.NodeID < 1 || !validSHA256Hex(version.BlobHash) ||
		version.Size < 0 || version.NodeRevision < 1 || !validUUIDv4(version.IntroducedOperationID) {
		return errors.New("invalid content version")
	}
	return nil
}

func emailPartRole(role string) bool {
	return document.IsEmailArtifactRole(role)
}

func emailArtifact(
	evidence document.EmailV1, partPath, role string,
) (document.EmailPartV1, document.EmailArtifactRefV1, error) {
	if evidence.Inventory == nil {
		return document.EmailPartV1{}, document.EmailArtifactRefV1{}, store.ErrEmailPartUnavailable
	}
	for _, part := range evidence.Inventory.Parts {
		if part.Path != partPath {
			continue
		}
		for _, artifact := range []*document.EmailArtifactRefV1{part.HeaderBlock, part.Payload, part.BodyUTF8} {
			if artifact != nil && string(artifact.Role) == role {
				return part, *artifact, nil
			}
		}
		return document.EmailPartV1{}, document.EmailArtifactRefV1{}, store.ErrEmailPartUnavailable
	}
	return document.EmailPartV1{}, document.EmailArtifactRefV1{}, store.ErrNotFound
}

func validateEmailPartHeaders(
	resp *http.Response, receipt api.EmailPartReceipt, expectedFilename string,
) error {
	if resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get(api.ContentVersionHeader) != receipt.Version.ID ||
		resp.Header.Get(api.EmailGenerationHeader) != receipt.GenerationID ||
		resp.Header.Get(api.EmailAttachmentHeader) != receipt.AttachmentID ||
		resp.Header.Get(api.EmailPartPathHeader) != receipt.PartPath ||
		resp.Header.Get(api.EmailPartRoleHeader) != receipt.Role ||
		resp.Header.Get(api.BlobHashHeader) != receipt.BlobSHA256 ||
		resp.Header.Get(api.BlobSizeHeader) != strconv.FormatInt(receipt.Size, 10) {
		return integrityErrorf("email part response headers do not match selected artifact")
	}
	disposition, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] == "" ||
		(expectedFilename != "" && params["filename"] != expectedFilename) {
		return integrityErrorf("email part response has invalid attachment disposition")
	}
	if _, announced := resp.Trailer["Content-Digest"]; !announced {
		return integrityErrorf("email part response does not announce Content-Digest trailer")
	}
	return nil
}

type verifiedEmailPartReader struct {
	readMu       sync.Mutex
	mu           sync.Mutex
	closeOnce    sync.Once
	body         io.ReadCloser
	trailer      http.Header
	expectedHash string
	expectedSize int64
	hash         hash.Hash
	read         int64
	verified     bool
	terminalErr  error
	closed       bool
	closeErr     error
}

func (r *verifiedEmailPartReader) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, errors.New("email part stream is closed")
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.mu.Unlock()
		return 0, err
	}
	body := r.body
	r.mu.Unlock()

	n, readErr := body.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return n, readErr
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	if n > 0 {
		r.read += int64(n)
		_, _ = r.hash.Write(p[:n])
		if r.read > r.expectedSize {
			r.terminalErr = integrityErrorf("verifying email part: received more than %d bytes", r.expectedSize)
			return n, r.terminalErr
		}
	}
	if errors.Is(readErr, io.EOF) {
		r.terminalErr = r.verify()
		if r.terminalErr != nil {
			return n, r.terminalErr
		}
		r.verified = true
		return n, io.EOF
	}
	if readErr != nil {
		r.terminalErr = fmt.Errorf("reading email part: %w", readErr)
	}
	return n, readErr
}

func (r *verifiedEmailPartReader) verify() error {
	if r.read != r.expectedSize {
		return integrityErrorf("verifying email part: received %d bytes, expected %d", r.read, r.expectedSize)
	}
	actual := r.hash.Sum(nil)
	if hex.EncodeToString(actual) != r.expectedHash {
		return integrityErrorf("verifying email part: computed SHA-256 does not match selected artifact")
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(actual) + ":"
	if r.trailer.Get("Content-Digest") != wantDigest {
		return integrityErrorf("verifying email part: terminal Content-Digest does not match delivered bytes")
	}
	return nil
}

func (r *verifiedEmailPartReader) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		verified, terminalErr := r.verified, r.terminalErr
		r.mu.Unlock()

		bodyErr := r.body.Close()
		r.mu.Lock()
		switch {
		case verified:
			r.closeErr = bodyErr
		case terminalErr != nil:
			r.closeErr = errors.Join(terminalErr, bodyErr)
		default:
			r.closeErr = errors.Join(integrityErrorf(
				"verifying email part: transfer closed before verified EOF"), bodyErr)
		}
		r.mu.Unlock()
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

var _ io.ReadCloser = (*verifiedEmailPartReader)(nil)
