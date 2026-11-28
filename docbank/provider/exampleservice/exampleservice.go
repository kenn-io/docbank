// Package exampleservice is a helper for a pretend hosting service. It keeps
// the service's whole state in one local JSON file and serves no documents, so
// a helper author or a test can run Docbank's provider commands without a
// real service. The approve and decline commands stand in for the person
// deciding in a browser.
package exampleservice

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/docbank/docbank/provider"
)

// Lifetimes the example service gives a credential and an approval.
const (
	CredentialLifetime = 15 * time.Minute
	ApprovalLifetime   = 90 * 24 * time.Hour
)

// Reasons the example service gives with a refusal or conflict.
const (
	ReasonVaultUnknown      = "vault_unknown"
	ReasonApprovalDeclined  = "approval_declined"
	ReasonApprovalExpired   = "approval_expired"
	ReasonAlreadyIssued     = "already_issued"
	ReasonGenerationChanged = "generation_changed"
	ReasonOpenReleased      = "open_released"
	ReasonOpenUnknown       = "open_unknown"
)

// State is everything the example service remembers.
type State struct {
	Vaults []provider.Vault `json:"vaults"`
	// Approvals are keyed by installation and vault.
	Approvals map[string]Approval `json:"approvals"`
	// Pending and Opens are keyed by the open's request ID.
	Pending map[uuid.UUID]Pending `json:"pending"`
	Opens   map[uuid.UUID]Open    `json:"opens"`
}

// Approval is the person's standing decision for one installation and vault.
type Approval struct {
	Actions   []provider.Action `json:"actions"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// Pending is an open waiting for the person. Declined records a refusal.
type Pending struct {
	InstallationID uuid.UUID         `json:"installation_id"`
	VaultID        uuid.UUID         `json:"vault_id"`
	Actions        []provider.Action `json:"actions"`
	Declined       bool              `json:"declined,omitempty"`
}

// Open is one issued open and its newest credential generation.
type Open struct {
	InstallationID uuid.UUID         `json:"installation_id"`
	VaultID        uuid.UUID         `json:"vault_id"`
	Actions        []provider.Action `json:"actions"`
	Generation     int64             `json:"generation"`
	ExpiresAt      time.Time         `json:"expires_at"`
	Released       bool              `json:"released,omitempty"`
}

// Run is the example helper's command line. With no command it answers one
// helper request from stdin. It returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("example-helper", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", "", "the example service's state `file`")
	browser := flags.String("browser-origin", "", "HTTPS `origin` of approval pages (default: the request's origin)")
	name := flags.String("vault", "Example documents", "`name` of the vault that init creates")
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, `Usage: example-helper --state FILE [init | approve | decline]

With no command, answers one provider helper request from stdin.
  init      create the state file with one vault
  approve   approve every waiting open, as the person would in a browser
  decline   decline every waiting open
`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *path == "" || flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	var err error
	switch flags.Arg(0) {
	case "":
		return answer(*path, *browser, stdin, stdout)
	case "init":
		err = update(*path, true, func(state *State) error {
			state.Vaults = []provider.Vault{{ID: uuid.New(), Name: *name, VaultUUID: uuid.New()}}
			return nil
		})
	case "approve", "decline":
		err = update(*path, false, func(state *State) error {
			return decide(state, flags.Arg(0) == "approve", time.Now(), stdout)
		})
	default:
		flags.Usage()
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// answer serves one helper request under the contract's exit codes.
func answer(path, browserOrigin string, stdin io.Reader, stdout io.Writer) int {
	request, err := provider.DecodeRequest(stdin)
	if err != nil {
		return 2
	}
	var response provider.Response
	err = update(path, false, func(state *State) error {
		response, err = Answer(state, request, cmpOr(browserOrigin, request.Origin), time.Now())
		return err
	})
	if err != nil || provider.WriteResponse(stdout, request, response) != nil {
		return 1
	}
	return 0
}

func cmpOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// Answer applies one request to state and returns the service's result.
// Approval pages are addressed under browserOrigin.
func Answer(state *State, request provider.Request, browserOrigin string, now time.Time) (provider.Response, error) {
	now = now.UTC().Truncate(time.Second)
	response := provider.Response{Version: provider.Version, Operation: request.Operation, RequestID: request.RequestID}
	if request.Operation == provider.OperationDiscover {
		response.Status = provider.StatusReady
		response.Discovery = &provider.Discovery{Enabled: true, Vaults: slices.Clone(state.Vaults)}
		return response, nil
	}
	known := slices.ContainsFunc(state.Vaults, func(vault provider.Vault) bool {
		return vault.ID == request.VaultID && vault.VaultUUID == request.VaultUUID
	})
	if !known {
		response.Status, response.Reason = provider.StatusDenied, ReasonVaultUnknown
		return response, nil
	}
	switch request.Operation {
	case provider.OperationOpen:
		return state.open(request, response, browserOrigin, now)
	case provider.OperationRenew:
		return state.renew(request, response, now), nil
	default:
		return state.release(request, response), nil
	}
}

func approvalKey(installation, vault uuid.UUID) string {
	return installation.String() + "/" + vault.String()
}

// open answers an open by its request ID, so asking again with the same
// request returns the same waiting page, then the person's decision.
func (state *State) open(request provider.Request, response provider.Response, browserOrigin string, now time.Time) (provider.Response, error) {
	key := approvalKey(request.InstallationID, request.VaultID)
	if open, ok := state.Opens[request.RequestID]; ok {
		response.Status, response.Reason = provider.StatusConflict, ReasonAlreadyIssued
		response.Access = state.access(request, open, key)
		return response, nil
	}
	approval, approved := state.Approvals[key]
	approved = approved && now.Before(approval.ExpiresAt) && covers(approval.Actions, request.Actions)
	pending, waiting := state.Pending[request.RequestID]
	switch {
	case waiting && pending.Declined:
		response.Status, response.Reason = provider.StatusDenied, ReasonApprovalDeclined
		return response, nil
	case !approved:
		if !waiting {
			state.Pending[request.RequestID] = Pending{
				InstallationID: request.InstallationID, VaultID: request.VaultID, Actions: request.Actions,
			}
		}
		response.Status = provider.StatusApprovalRequired
		response.ApprovalURL = browserOrigin + "/approve/" + url.PathEscape(request.RequestID.String())
		return response, nil
	}
	delete(state.Pending, request.RequestID)
	open := Open{
		InstallationID: request.InstallationID, VaultID: request.VaultID, Actions: request.Actions,
		Generation: 1, ExpiresAt: earlier(now.Add(CredentialLifetime), approval.ExpiresAt),
	}
	state.Opens[request.RequestID] = open
	return issue(response, state.access(request, open, key))
}

func (state *State) renew(request provider.Request, response provider.Response, now time.Time) provider.Response {
	key := approvalKey(request.InstallationID, request.VaultID)
	open, ok := state.Opens[request.OpenRequestID]
	approval := state.Approvals[key]
	switch {
	case !ok || open.InstallationID != request.InstallationID || open.VaultID != request.VaultID ||
		!slices.Equal(open.Actions, request.Actions):
		response.Status, response.Reason = provider.StatusDenied, ReasonOpenUnknown
	case open.Released:
		response.Status, response.Reason = provider.StatusDenied, ReasonOpenReleased
	case !now.Before(approval.ExpiresAt) || !covers(approval.Actions, open.Actions):
		response.Status, response.Reason = provider.StatusDenied, ReasonApprovalExpired
	case open.Generation != request.Generation:
		response.Status, response.Reason = provider.StatusConflict, ReasonGenerationChanged
		response.Access = state.access(request, open, key)
	default:
		open.Generation++
		open.ExpiresAt = earlier(now.Add(CredentialLifetime), approval.ExpiresAt)
		state.Opens[request.OpenRequestID] = open
		issued, err := issue(response, state.access(request, open, key))
		if err != nil {
			response.Status, response.Reason = provider.StatusUnavailable, ""
			return response
		}
		return issued
	}
	return response
}

func (state *State) release(request provider.Request, response provider.Response) provider.Response {
	open, ok := state.Opens[request.OpenRequestID]
	if !ok || open.InstallationID != request.InstallationID || open.VaultID != request.VaultID {
		response.Status, response.Reason = provider.StatusDenied, ReasonOpenUnknown
		return response
	}
	open.Released = true
	state.Opens[request.OpenRequestID] = open
	response.Status = provider.StatusReleased
	return response
}

// access describes an open without its credential.
func (state *State) access(request provider.Request, open Open, key string) *provider.Access {
	mount := url.URL{Path: strings.TrimSuffix(request.MountPrefix, "/") + "/" + open.VaultID.String()}
	return &provider.Access{
		Actions: open.Actions, Mount: request.Origin + mount.EscapedPath(), Generation: open.Generation,
		ExpiresAt: open.ExpiresAt, ApprovalExpiresAt: state.Approvals[key].ExpiresAt,
	}
}

// issue adds a new credential to a ready result. The example service serves
// no documents, so it keeps no record of the credential.
func issue(response provider.Response, access *provider.Access) (provider.Response, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return provider.Response{}, fmt.Errorf("making a credential: %w", err)
	}
	access.Credential = "example_" + base64.RawURLEncoding.EncodeToString(secret)
	response.Status, response.Access = provider.StatusReady, access
	return response, nil
}

// decide records the person's answer to every waiting open. Approving covers
// the actions each open asked for, replacing an earlier approval for that
// installation and vault.
func decide(state *State, approve bool, now time.Time, stdout io.Writer) error {
	now = now.UTC().Truncate(time.Second)
	decided := 0
	for id, pending := range state.Pending {
		if pending.Declined {
			continue
		}
		decided++
		if approve {
			state.Approvals[approvalKey(pending.InstallationID, pending.VaultID)] = Approval{
				Actions: pending.Actions, ExpiresAt: now.Add(ApprovalLifetime),
			}
			continue
		}
		pending.Declined = true
		state.Pending[id] = pending
	}
	word := "Declined"
	if approve {
		word = "Approved"
	}
	if _, err := fmt.Fprintf(stdout, "%s %d waiting open(s)\n", word, decided); err != nil {
		return fmt.Errorf("reporting the decision: %w", err)
	}
	return nil
}

func covers(approved, wanted []provider.Action) bool {
	for _, action := range wanted {
		if !slices.Contains(approved, action) {
			return false
		}
	}
	return true
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Load reads the state file, waiting for a helper that is writing it.
func Load(path string) (State, error) {
	var loaded State
	err := update(path, false, func(state *State) error {
		loaded = *state
		return errUnchanged
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return loaded, err
}

// errUnchanged ends an update without saving.
var errUnchanged = errors.New("state unchanged")

// update loads the state file, applies change, and saves the result, holding
// a lock so helper processes that run at once do not lose each other's
// writes. create starts from an empty state and refuses to replace a file.
func update(path string, create bool, change func(*State) error) error {
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer func() { _ = lock.Unlock() }()
	state := State{}
	data, err := os.ReadFile(path)
	switch {
	case create && err == nil:
		return fmt.Errorf("%s already exists", path)
	case create && errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("reading the example service's state (run init first): %w", err)
	default:
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if state.Approvals == nil {
		state.Approvals = map[string]Approval{}
	}
	if state.Pending == nil {
		state.Pending = map[uuid.UUID]Pending{}
	}
	if state.Opens == nil {
		state.Opens = map[uuid.UUID]Open{}
	}
	if err := change(&state); err != nil {
		return err
	}
	data, err = json.Marshal(state)
	if err != nil {
		return err
	}
	next, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(next.Name()) }()
	if _, err := next.Write(data); err != nil {
		_ = next.Close()
		return err
	}
	if err := next.Close(); err != nil {
		return err
	}
	if err := atomicfile.Replace(next.Name(), path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}
