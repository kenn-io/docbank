// Package provider defines the exchange between Docbank and a credential
// helper for hosted vaults. A helper is a trusted local executable that speaks
// for the person signed in to the hosting service. Docbank sends one JSON
// request on the helper's stdin and reads one JSON result from its stdout.
//
// A discovery lists the vaults the person can open. Opening one vault returns
// a short-lived credential for it, which a renewal replaces before it expires
// and a release ends.
//
// Helper authors read the request with DecodeRequest and answer with
// WriteResponse. Docbank runs the helper with Exchange. See
// docs/usage/providers.md for the configuration and the wire contract.
package provider

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/kit/consolewindow"
)

// Version is the only exchange version this package reads and writes.
const Version = 1

// MaxRequestBytes bounds one request document.
const MaxRequestBytes = 16 << 10

// MaxResponseBytes bounds one result document, which can list many vaults.
const MaxResponseBytes = 256 << 10

// AttemptTimeout bounds one helper run unless the caller's context ends first.
const AttemptTimeout = 60 * time.Second

// Operation names the requested exchange.
type Operation string

// Operations a request can name.
const (
	// OperationDiscover lists the vaults the signed-in person can open.
	OperationDiscover Operation = "discover"
	// OperationOpen asks for a credential for one vault.
	OperationOpen Operation = "open"
	// OperationRenew replaces an open vault's credential before it expires.
	OperationRenew Operation = "renew"
	// OperationRelease ends an open, so its credentials stop working.
	OperationRelease Operation = "release"
)

// Action is one kind of access to a vault.
type Action string

// Actions an open can ask for.
const (
	ActionRead   Action = "read"
	ActionUpload Action = "upload"
)

// MaxCredentialBytes bounds one credential.
const MaxCredentialBytes = 4096

// Status is the hosting service's decision, separate from the helper's exit code.
type Status string

// Statuses a result can carry. Every operation can return the last three.
const (
	// StatusReady answers a discovery with its vaults, and an open or renewal
	// with a credential.
	StatusReady Status = "ready"
	// StatusApprovalRequired answers an open the person has not approved yet.
	// The result names the page where they decide.
	StatusApprovalRequired Status = "approval_required"
	// StatusConflict answers an open or renewal the service already answered
	// with a credential. It describes that access without the credential.
	StatusConflict Status = "conflict"
	// StatusReleased answers a release.
	StatusReleased       Status = "released"
	StatusSignInRequired Status = "sign_in_required"
	StatusDenied         Status = "denied"
	StatusUnavailable    Status = "unavailable"
)

var (
	// ErrInvalidRequest reports a malformed request without its contents.
	ErrInvalidRequest = errors.New("invalid provider request")
	// ErrInvalidResponse reports a result that does not answer the request.
	ErrInvalidResponse = errors.New("invalid provider response")
	// ErrHelperFailed reports a helper that did not complete the exchange.
	ErrHelperFailed = errors.New("provider helper failed")
)

// Connection identifies the hosting service a request is for. Origin is an
// HTTPS origin; MountPrefix is the absolute path under which it serves vaults.
type Connection struct {
	InstallationID uuid.UUID `json:"installation_id"`
	Origin         string    `json:"origin"`
	MountPrefix    string    `json:"mount_prefix"`
}

// Request is one helper exchange. An open, renewal, or release names one vault
// by both identities and the actions it is for. A renewal or release also names
// the open by that open's request ID; a renewal carries the generation of the
// credential it replaces.
type Request struct {
	Connection

	Version   int       `json:"version"`
	Operation Operation `json:"operation"`
	RequestID uuid.UUID `json:"request_id"`

	VaultID       uuid.UUID `json:"vault_id,omitzero"`
	VaultUUID     uuid.UUID `json:"vault_uuid,omitzero"`
	Actions       []Action  `json:"actions,omitempty"`
	OpenRequestID uuid.UUID `json:"open_request_id,omitzero"`
	Generation    int64     `json:"generation,omitzero"`
}

// Vault is one hosted vault the person can open. ID is the hosting service's
// stable identity for the vault; VaultUUID is the Docbank vault identity.
type Vault struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	VaultUUID uuid.UUID `json:"vault_uuid"`
}

// Discovery is a ready discovery result. Enabled reports whether the service
// offers vaults at all; CanCreate reports whether this person can create one.
type Discovery struct {
	Enabled   bool    `json:"enabled"`
	CanCreate bool    `json:"can_create"`
	Vaults    []Vault `json:"vaults"`
}

// Access is what an open or renewal grants. Mount is the HTTPS address of the
// vault, under the connection's mount prefix. Generation counts the open's
// credentials from 1; a newer generation replaces every older one. The
// credential stops working at ExpiresAt, and the person's approval ends at
// ApprovalExpiresAt. A conflict carries the same facts without Credential.
type Access struct {
	Actions           []Action  `json:"actions"`
	Mount             string    `json:"mount"`
	Credential        string    `json:"credential,omitempty"`
	Generation        int64     `json:"generation"`
	ExpiresAt         time.Time `json:"expires_at"`
	ApprovalExpiresAt time.Time `json:"approval_expires_at"`
}

// Response is the helper's result. A ready discovery carries Discovery. A ready
// or conflicting open or renewal carries Access. An open that needs approval
// carries ApprovalURL. Any other status carries only the envelope and an
// optional fixed Reason code.
type Response struct {
	Version     int        `json:"version"`
	Operation   Operation  `json:"operation"`
	RequestID   uuid.UUID  `json:"request_id"`
	Status      Status     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	Discovery   *Discovery `json:"discovery,omitempty"`
	Access      *Access    `json:"access,omitempty"`
	ApprovalURL string     `json:"approval_url,omitempty"`
}

// VaultAccess names one vault and the actions an open is for.
type VaultAccess struct {
	VaultID   uuid.UUID
	VaultUUID uuid.UUID
	Actions   []Action
}

// NewDiscoverRequest returns a discovery request with a fresh request ID.
func NewDiscoverRequest(connection Connection) (Request, error) {
	request := Request{Connection: connection, Version: Version, Operation: OperationDiscover, RequestID: uuid.New()}
	if err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

// NewOpenRequest returns a request to open one vault, with a fresh request ID
// that later renewals and the release name.
func NewOpenRequest(connection Connection, access VaultAccess) (Request, error) {
	return newVaultRequest(connection, OperationOpen, access, uuid.Nil(), 0)
}

// NewRenewRequest returns a request to replace the credential of generation
// that the open returned or a renewal after it.
func NewRenewRequest(open Request, generation int64) (Request, error) {
	if open.Operation != OperationOpen || validateRequest(open) != nil {
		return Request{}, ErrInvalidRequest
	}
	access := VaultAccess{VaultID: open.VaultID, VaultUUID: open.VaultUUID, Actions: open.Actions}
	return newVaultRequest(open.Connection, OperationRenew, access, open.RequestID, generation)
}

// NewReleaseRequest returns a request to end open.
func NewReleaseRequest(open Request) (Request, error) {
	if open.Operation != OperationOpen || validateRequest(open) != nil {
		return Request{}, ErrInvalidRequest
	}
	access := VaultAccess{VaultID: open.VaultID, VaultUUID: open.VaultUUID, Actions: open.Actions}
	return newVaultRequest(open.Connection, OperationRelease, access, open.RequestID, 0)
}

func newVaultRequest(connection Connection, operation Operation, access VaultAccess, open uuid.UUID, generation int64) (Request, error) {
	request := Request{
		Connection: connection, Version: Version, Operation: operation, RequestID: uuid.New(),
		VaultID: access.VaultID, VaultUUID: access.VaultUUID, Actions: slices.Clone(access.Actions),
		OpenRequestID: open, Generation: generation,
	}
	if err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

// DecodeRequest reads and checks one request. A helper exits 2 when this
// fails, before it contacts the hosting service.
func DecodeRequest(reader io.Reader) (Request, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil || len(data) > MaxRequestBytes {
		return Request{}, ErrInvalidRequest
	}
	var request Request
	if err := unmarshalStrict(data, &request); err != nil {
		return Request{}, ErrInvalidRequest
	}
	if err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

// DecodeResponse reads one result and checks that it answers request.
func DecodeResponse(reader io.Reader, request Request) (Response, error) {
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxResponseBytes+1))
	if err != nil || len(data) > MaxResponseBytes {
		return Response{}, ErrInvalidResponse
	}
	return decodeResponse(data, request)
}

// WriteResponse checks response against request and writes it. A helper that
// cannot write its result exits nonzero.
func WriteResponse(writer io.Writer, request Request, response Response) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	if err := validateResponse(response, request); err != nil {
		return err
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) > MaxResponseBytes {
		return ErrInvalidResponse
	}
	_, err = writer.Write(data)
	return err
}

// Exchange runs the configured helper argv without a shell and returns its
// checked result. The helper runs as trusted code under the same user and
// inherits the caller's environment. Its stderr is discarded, and its stdout
// is discarded when it exits nonzero. A refusal is a valid result, not an error.
func Exchange(parent context.Context, argv []string, request Request) (Response, error) {
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	if err := ValidateHelper(argv); err != nil {
		return Response{}, err
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > MaxRequestBytes {
		return Response{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(parent, AttemptTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv comes from the person's own provider configuration.
	consolewindow.Hide(command)
	command.Stdin = bytes.NewReader(data)
	// Bound the wait for a descendant that keeps stdout open after the helper exits.
	command.WaitDelay = time.Second
	out := &boundedOutput{cancel: cancel}
	command.Stdout = out
	runErr := command.Run()
	switch {
	case out.overflow:
		return Response{}, ErrInvalidResponse
	case ctx.Err() != nil:
		return Response{}, errors.Join(ErrHelperFailed, ctx.Err())
	case runErr != nil:
		if exit, ok := errors.AsType[*exec.ExitError](runErr); ok && exit.ExitCode() == 2 {
			return Response{}, fmt.Errorf("%w: the helper rejected the request", ErrHelperFailed)
		}
		return Response{}, ErrHelperFailed
	}
	return decodeResponse(out.buffer.Bytes(), request)
}

// ValidateHelper checks a configured helper command: an absolute executable
// path followed by its arguments.
func ValidateHelper(argv []string) error {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return errors.New("provider helper must start with an absolute executable path")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return errors.New("provider helper arguments must be UTF-8 text without a zero byte")
		}
	}
	return nil
}

// ValidateConnection checks the origin and mount prefix of a connection.
func ValidateConnection(origin, mountPrefix string) error {
	if err := ValidateOrigin(origin); err != nil {
		return err
	}
	if mountPrefix == "" || mountPrefix[0] != '/' || path.Clean(mountPrefix) != mountPrefix ||
		strings.ContainsAny(mountPrefix, "?#%\\") || !utf8.ValidString(mountPrefix) {
		return fmt.Errorf("mount prefix %q must be a clean absolute path without a trailing slash", mountPrefix)
	}
	return nil
}

// ValidateOrigin requires an HTTPS origin: scheme and host only, with no path,
// query, fragment, or user information.
func ValidateOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		strings.HasSuffix(origin, "?") || strings.HasSuffix(origin, "#") || origin != parsed.String() {
		return fmt.Errorf("origin %q must be an HTTPS origin such as https://vaults.example.org", origin)
	}
	return nil
}

// ValidateApprovalURL checks the page a service names for approving an open:
// an HTTPS address at browserOrigin, the origin the person's browser uses for
// the service, with no user information or fragment.
func ValidateApprovalURL(approvalURL, browserOrigin string) error {
	if err := ValidateOrigin(browserOrigin); err != nil {
		return err
	}
	parsed, err := url.Parse(approvalURL)
	if err != nil || validApprovalURL(approvalURL) != nil || parsed.Scheme+"://"+parsed.Host != browserOrigin {
		return fmt.Errorf("approval address must be an HTTPS page at %s", browserOrigin)
	}
	return nil
}

func validApprovalURL(approvalURL string) error {
	parsed, err := url.Parse(approvalURL)
	if err != nil || len(approvalURL) > 2048 || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || strings.HasSuffix(approvalURL, "#") {
		return ErrInvalidResponse
	}
	return nil
}

func validateRequest(request Request) error {
	if request.Version != Version || request.RequestID == uuid.Nil() || request.InstallationID == uuid.Nil() ||
		ValidateConnection(request.Origin, request.MountPrefix) != nil {
		return ErrInvalidRequest
	}
	names := request.VaultID != uuid.Nil() && request.VaultUUID != uuid.Nil() && validActions(request.Actions)
	var valid bool
	switch request.Operation {
	case OperationDiscover:
		valid = request.VaultID == uuid.Nil() && request.VaultUUID == uuid.Nil() && len(request.Actions) == 0 &&
			request.OpenRequestID == uuid.Nil() && request.Generation == 0
	case OperationOpen:
		valid = names && request.OpenRequestID == uuid.Nil() && request.Generation == 0
	case OperationRenew:
		valid = names && request.OpenRequestID != uuid.Nil() && request.Generation >= 1
	case OperationRelease:
		valid = names && request.OpenRequestID != uuid.Nil() && request.Generation == 0
	}
	if !valid {
		return ErrInvalidRequest
	}
	return nil
}

// Actions are a nonempty sorted list of known actions without repeats, so a
// request has one spelling.
func validActions(actions []Action) bool {
	for i, action := range actions {
		if (action != ActionRead && action != ActionUpload) || (i > 0 && actions[i-1] >= action) {
			return false
		}
	}
	return len(actions) > 0
}

func decodeResponse(data []byte, request Request) (Response, error) {
	var response Response
	if err := unmarshalStrict(data, &response); err != nil {
		return Response{}, ErrInvalidResponse
	}
	if err := validateResponse(response, request); err != nil {
		return Response{}, err
	}
	return response, nil
}

func validateResponse(response Response, request Request) error {
	if response.Version != Version || response.Operation != request.Operation || response.RequestID != request.RequestID {
		return ErrInvalidResponse
	}
	grants := request.Operation == OperationOpen || request.Operation == OperationRenew
	bare := response.Discovery == nil && response.Access == nil && response.ApprovalURL == ""
	switch response.Status {
	case StatusReady:
		if response.Reason != "" || response.ApprovalURL != "" {
			return ErrInvalidResponse
		}
		if grants {
			if response.Discovery != nil || response.Access == nil || !validCredential(response.Access.Credential) {
				return ErrInvalidResponse
			}
			return validateAccess(*response.Access, request)
		}
		if request.Operation != OperationDiscover || response.Access != nil ||
			response.Discovery == nil || response.Discovery.Vaults == nil {
			return ErrInvalidResponse
		}
		return validateVaults(response.Discovery.Vaults)
	case StatusConflict:
		if !grants || response.Discovery != nil || response.ApprovalURL != "" || !validReason(response.Reason) ||
			response.Access == nil || response.Access.Credential != "" {
			return ErrInvalidResponse
		}
		return validateAccess(*response.Access, request)
	case StatusApprovalRequired:
		if request.Operation != OperationOpen || response.Discovery != nil || response.Access != nil ||
			response.Reason != "" {
			return ErrInvalidResponse
		}
		return validApprovalURL(response.ApprovalURL)
	case StatusReleased:
		if request.Operation != OperationRelease || !bare || response.Reason != "" {
			return ErrInvalidResponse
		}
		return nil
	case StatusSignInRequired, StatusDenied, StatusUnavailable:
		if !bare || !validReason(response.Reason) {
			return ErrInvalidResponse
		}
		return nil
	default:
		return ErrInvalidResponse
	}
}

// validateAccess checks the facts a ready or conflicting open or renewal
// carries, apart from the credential.
func validateAccess(access Access, request Request) error {
	if !validActions(access.Actions) || access.Generation < 1 ||
		(request.Operation == OperationRenew && access.Generation <= request.Generation) ||
		!utcInstant(access.ExpiresAt) || !utcInstant(access.ApprovalExpiresAt) ||
		access.ExpiresAt.After(access.ApprovalExpiresAt) {
		return ErrInvalidResponse
	}
	for _, action := range access.Actions {
		if !slices.Contains(request.Actions, action) {
			return ErrInvalidResponse
		}
	}
	return validateMount(access.Mount, request.Connection)
}

// A mount is an address strictly under the connection's mount prefix, so a
// result cannot send the credential anywhere else. Its path is percent-encoded
// the one way net/url writes it, so a prefix with a space or non-ASCII letters
// has exactly one accepted spelling.
func validateMount(mount string, connection Connection) error {
	// The root prefix already ends in the slash that separates it from a vault.
	prefix := strings.TrimSuffix(connection.MountPrefix, "/") + "/"
	parsed, err := url.Parse(mount)
	if err != nil || parsed.Scheme+"://"+parsed.Host != connection.Origin || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || mount != parsed.String() ||
		strings.ContainsAny(parsed.Path, "%\\") || path.Clean(parsed.Path) != parsed.Path ||
		!strings.HasPrefix(parsed.Path, prefix) || parsed.Path == prefix ||
		mount != connection.Origin+(&url.URL{Path: parsed.Path}).EscapedPath() {
		return ErrInvalidResponse
	}
	return nil
}

func utcInstant(instant time.Time) bool {
	_, offset := instant.Zone()
	return !instant.IsZero() && offset == 0
}

// A credential is visible ASCII without spaces, so it fits an HTTP header.
func validCredential(credential string) bool {
	if credential == "" || len(credential) > MaxCredentialBytes {
		return false
	}
	for _, r := range credential {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

func validateVaults(vaults []Vault) error {
	ids := make(map[uuid.UUID]struct{}, len(vaults))
	for _, vault := range vaults {
		if vault.ID == uuid.Nil() || vault.VaultUUID == uuid.Nil() || !validName(vault.Name) {
			return ErrInvalidResponse
		}
		if _, duplicate := ids[vault.ID]; duplicate {
			return ErrInvalidResponse
		}
		ids[vault.ID] = struct{}{}
	}
	return nil
}

// A reason is a short fixed code, never free text from the service.
func validReason(reason string) bool {
	if len(reason) > 64 {
		return false
	}
	for _, r := range reason {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

func validName(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
			return false
		}
	}
	return true
}

func unmarshalStrict(data []byte, value any) error {
	return json.Unmarshal(data, value, json.RejectUnknownMembers(true), jsontext.AllowDuplicateNames(false))
}

// Do not embed bytes.Buffer: its ReadFrom would bypass the limit in io.Copy.
type boundedOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (o *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > MaxResponseBytes-o.buffer.Len() {
		o.overflow = true
		o.cancel()
		return 0, ErrInvalidResponse
	}
	n, _ := o.buffer.Write(data) // bytes.Buffer.Write never returns an error.
	return n, nil
}
