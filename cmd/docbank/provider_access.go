package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/docbank/provider"
	"go.kenn.io/docbank/internal/config"
)

var (
	providerAccessJSON    bool
	providerAccessActions string
	providerAccessWait    time.Duration
	// providerAccessPoll is how often the command asks again while the
	// person decides.
	providerAccessPoll = 2 * time.Second
	// providerAccessPause waits out one poll interval.
	providerAccessPause = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(providerAccessPoll):
			return nil
		}
	}
)

// providerAccess is what provider access reports. It never carries the
// credential.
type providerAccess struct {
	Vault             provider.Vault    `json:"vault"`
	Actions           []provider.Action `json:"actions"`
	Mount             string            `json:"mount"`
	ApprovalExpiresAt time.Time         `json:"approval_expires_at"`
}

var providerAccessCmd = &cobra.Command{
	Example: `  docbank provider access work "Studio documents"
  docbank provider access work "Studio documents" --actions read,upload --json`,
	Use:   "access <name> <vault>",
	Short: "Check that a provider lets this computer open a vault",
	Long: `Asks the provider's helper to open one vault, renews the credential once,
and ends the open. Name the vault by its ID or its exact name from
docbank provider vaults.

When the provider needs your approval, the command prints the page where you
decide and waits for --wait. It prints the vault's address and when your
approval ends, and never the credential.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		actions, err := providerActions(providerAccessActions)
		if err != nil {
			return usageError(err)
		}
		file, err := loadProviders()
		if err != nil {
			return err
		}
		entry, ok := file.Providers[args[0]]
		if !ok {
			return usageError(fmt.Errorf("no provider named %q; run docbank provider list", args[0]))
		}
		connection := config.HostedProviderConnection(file, entry)
		discovery, err := discoverVaults(cmd.Context(), args[0], connection, entry.Helper)
		if err != nil {
			return err
		}
		vault, err := chooseProviderVault(discovery.Vaults, args[0], args[1])
		if err != nil {
			return usageError(err)
		}
		check := providerAccessCheck{name: args[0], helper: entry.Helper, browserOrigin: entry.BrowserOrigin, cmd: cmd}
		access, err := check.run(cmd.Context(), connection, provider.VaultAccess{VaultID: vault.ID, VaultUUID: vault.VaultUUID, Actions: actions})
		if err != nil {
			return err
		}
		result := providerAccess{Vault: vault, Actions: access.Actions, Mount: access.Mount, ApprovalExpiresAt: access.ApprovalExpiresAt}
		if providerAccessJSON {
			return writeCLIJSON(cmd.OutOrStdout(), result)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Provider %s lets this computer %s %s\nAddress: %s\nApproval ends: %s\n",
			args[0], actionWords(access.Actions), vault.Name, access.Mount, access.ApprovalExpiresAt.Format(time.RFC3339))
		return nil
	},
}

// providerActions checks --actions and puts it in the contract's order.
func providerActions(list string) ([]provider.Action, error) {
	var actions []provider.Action
	for name := range strings.SplitSeq(list, ",") {
		action := provider.Action(name)
		if action != provider.ActionRead && action != provider.ActionUpload {
			return nil, fmt.Errorf("unknown action %q; use read or upload", name)
		}
		if !slices.Contains(actions, action) {
			actions = append(actions, action)
		}
	}
	slices.Sort(actions)
	return actions, nil
}

func actionWords(actions []provider.Action) string {
	words := make([]string, len(actions))
	for i, action := range actions {
		words[i] = string(action)
	}
	if actions[len(actions)-1] == provider.ActionUpload {
		words[len(words)-1] = "upload to"
	}
	return strings.Join(words, " and ")
}

// chooseProviderVault finds the vault named by its ID or its exact name.
func chooseProviderVault(vaults []provider.Vault, name, wanted string) (provider.Vault, error) {
	id, _ := uuid.Parse(wanted)
	var found []provider.Vault
	for _, vault := range vaults {
		if (id != uuid.Nil() && vault.ID == id) || vault.Name == wanted {
			found = append(found, vault)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return provider.Vault{}, fmt.Errorf("provider %s has no vault %q; run docbank provider vaults %s", name, wanted, name)
	default:
		return provider.Vault{}, fmt.Errorf("provider %s has %d vaults named %q; name one by its ID", name, len(found), wanted)
	}
}

type providerAccessCheck struct {
	name          string
	helper        []string
	browserOrigin string
	cmd           *cobra.Command
}

// run opens the vault, renews its credential once, and ends the open. It
// returns the renewed access.
func (check providerAccessCheck) run(ctx context.Context, connection provider.Connection, vault provider.VaultAccess) (provider.Access, error) {
	open, err := provider.NewOpenRequest(connection, vault)
	if err != nil {
		return provider.Access{}, err
	}
	opened, err := check.open(ctx, open)
	if err != nil {
		return provider.Access{}, err
	}
	// The open exists on the service from here, so end it even when the
	// command is interrupted or the renewal fails.
	renewed, err := check.renew(ctx, open, opened.Generation)
	if releaseErr := check.release(context.WithoutCancel(ctx), open); err == nil {
		err = releaseErr
	}
	return renewed, err
}

// open asks for the open, and asks again with the same request while the
// person decides, so the service keeps showing them one page.
func (check providerAccessCheck) open(ctx context.Context, open provider.Request) (provider.Access, error) {
	deadline := time.Now().Add(providerAccessWait)
	shown := ""
	for {
		response, err := check.ask(ctx, open, "open")
		if err != nil {
			return provider.Access{}, err
		}
		if response.Status == provider.StatusReady {
			return *response.Access, nil
		}
		if response.Status != provider.StatusApprovalRequired {
			return provider.Access{}, check.refused(response)
		}
		if err := provider.ValidateApprovalURL(response.ApprovalURL, check.browserOrigin); err != nil {
			return provider.Access{}, fmt.Errorf("provider %s named an approval page Docbank does not trust: %w", check.name, err)
		}
		if !time.Now().Add(providerAccessPoll).Before(deadline) {
			return provider.Access{}, fmt.Errorf("provider %s is waiting for your approval at %s; approve there, then run this command again",
				check.name, response.ApprovalURL)
		}
		if response.ApprovalURL != shown {
			shown = response.ApprovalURL
			_, _ = fmt.Fprintf(check.cmd.ErrOrStderr(), "Approve this request in your browser:\n  %s\n", shown)
		}
		if err := providerAccessPause(ctx); err != nil {
			return provider.Access{}, err
		}
	}
}

func (check providerAccessCheck) renew(ctx context.Context, open provider.Request, generation int64) (provider.Access, error) {
	renew, err := provider.NewRenewRequest(open, generation)
	if err != nil {
		return provider.Access{}, err
	}
	response, err := check.ask(ctx, renew, "renew")
	if err != nil {
		return provider.Access{}, err
	}
	if response.Status != provider.StatusReady {
		return provider.Access{}, check.refused(response)
	}
	return *response.Access, nil
}

func (check providerAccessCheck) release(ctx context.Context, open provider.Request) error {
	release, err := provider.NewReleaseRequest(open)
	if err != nil {
		return err
	}
	response, err := check.ask(ctx, release, "release")
	if err != nil {
		return err
	}
	if response.Status != provider.StatusReleased {
		return check.refused(response)
	}
	return nil
}

func (check providerAccessCheck) ask(ctx context.Context, request provider.Request, verb string) (provider.Response, error) {
	response, err := provider.Exchange(ctx, check.helper, request)
	if err != nil {
		return provider.Response{}, fmt.Errorf("asking provider %s to %s the vault: %w", check.name, verb, err)
	}
	return response, nil
}

// refused explains a result that grants nothing.
func (check providerAccessCheck) refused(response provider.Response) error {
	switch response.Status {
	case provider.StatusSignInRequired:
		return fmt.Errorf("provider %s needs you to sign in to its service; sign in there, then retry", check.name)
	case provider.StatusDenied:
		return fmt.Errorf("provider %s denied the %s%s", check.name, response.Operation, reasonSuffix(response.Reason))
	case provider.StatusUnavailable:
		return fmt.Errorf("provider %s is unavailable%s; retry later", check.name, reasonSuffix(response.Reason))
	case provider.StatusConflict:
		return fmt.Errorf("provider %s already answered this %s%s; run the command again", check.name, response.Operation, reasonSuffix(response.Reason))
	default:
		return errors.New("provider " + check.name + " gave an answer Docbank does not expect")
	}
}

func init() {
	providerAccessCmd.Flags().BoolVar(&providerAccessJSON, "json", false, "print JSON to stdout")
	providerAccessCmd.Flags().StringVar(&providerAccessActions, "actions", "read", "what to ask for: read, upload, or read,upload")
	providerAccessCmd.Flags().DurationVar(&providerAccessWait, "wait", 5*time.Minute, "how long to wait for your approval; 0 does not wait")
	providerCmd.AddCommand(providerAccessCmd)
}
