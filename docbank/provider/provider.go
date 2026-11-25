// Package provider defines the exchange between Docbank and a credential
// helper for hosted vaults. A helper is a trusted local executable that speaks
// for the person signed in to the hosting service. Docbank sends one JSON
// request on the helper's stdin and reads one JSON result from its stdout.
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

// OperationDiscover lists the vaults the signed-in person can open.
const OperationDiscover Operation = "discover"

// Status is the hosting service's decision, separate from the helper's exit code.
type Status string

// Statuses a discovery can return.
const (
	StatusReady          Status = "ready"
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

// Request is one helper exchange.
type Request struct {
	Connection

	Version   int       `json:"version"`
	Operation Operation `json:"operation"`
	RequestID uuid.UUID `json:"request_id"`
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

// Response is the helper's result. A ready discovery carries Discovery; any
// other status carries only the envelope and an optional fixed Reason code.
type Response struct {
	Version   int        `json:"version"`
	Operation Operation  `json:"operation"`
	RequestID uuid.UUID  `json:"request_id"`
	Status    Status     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Discovery *Discovery `json:"discovery,omitempty"`
}

// NewDiscoverRequest returns a discovery request with a fresh request ID.
func NewDiscoverRequest(connection Connection) (Request, error) {
	request := Request{Connection: connection, Version: Version, Operation: OperationDiscover, RequestID: uuid.New()}
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

func validateRequest(request Request) error {
	if request.Version != Version || request.Operation != OperationDiscover ||
		request.RequestID == uuid.Nil() || request.InstallationID == uuid.Nil() ||
		ValidateConnection(request.Origin, request.MountPrefix) != nil {
		return ErrInvalidRequest
	}
	return nil
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
	switch response.Status {
	case StatusReady:
		if response.Reason != "" || response.Discovery == nil || response.Discovery.Vaults == nil {
			return ErrInvalidResponse
		}
		return validateVaults(response.Discovery.Vaults)
	case StatusSignInRequired, StatusDenied, StatusUnavailable:
		if response.Discovery != nil || !validReason(response.Reason) {
			return ErrInvalidResponse
		}
		return nil
	default:
		return ErrInvalidResponse
	}
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
