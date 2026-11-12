package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func contractDate(d *string) string {
	if d == nil {
		return "-"
	}
	return *d
}
func printContract(cmd *cobra.Command, c api.Contract, jsonOutput bool) error {
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), c)
	}
	parties := []string{}
	for _, p := range c.Parties {
		parties = append(parties, p.Role+":"+p.DisplayName)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\t%s\trevision %d\n", c.ContractID, c.Title, c.ContractType, c.EffectiveStatus, contractDate(c.StartsOn), contractDate(c.ExpiresOn), strings.Join(parties, ", "), c.Revision)
	if err != nil {
		return fmt.Errorf("write contract: %w", err)
	}
	return nil
}
func printContractShow(cmd *cobra.Command, c api.Contract, jsonOutput bool) error {
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), c)
	}
	if err := printContract(cmd, c, false); err != nil {
		return err
	}
	for _, p := range c.Parties {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Party: %s %s (%s)\n", p.Role, p.DisplayName, p.PartyID); err != nil {
			return fmt.Errorf("write contract party: %w", err)
		}
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Stored status: %s\nSigned: %s\nCreated: %s\nUpdated: %s\nSummary: %q\nPurpose: %q\nConsideration: %q\nTermination: %q\n", c.Status, contractDate(c.SignedOn), c.CreatedAt, c.UpdatedAt, c.Summary, c.Purpose, c.Consideration, c.Termination)
	if err != nil {
		return fmt.Errorf("write contract details: %w", err)
	}
	return nil
}
func resolveLinkedContractParty(c api.Contract, role, name string) (string, error) {
	if id, err := uuid.Parse(name); err == nil && id.String() == name {
		for _, p := range c.Parties {
			if p.PartyID == name && p.Role == role {
				return name, nil
			}
		}
		return "", fmt.Errorf("linked party %q not found for role %s", name, role)
	}
	var found string
	for _, p := range c.Parties {
		if p.Role == role && document.FoldPersonName(p.DisplayName) == document.FoldPersonName(name) {
			if found != "" {
				return "", fmt.Errorf("party %q is ambiguous; use an ID", name)
			}
			found = p.PartyID
		}
	}
	if found == "" {
		return "", fmt.Errorf("linked party %q not found for role %s", name, role)
	}
	return found, nil
}
func resolveContractParty(ctx context.Context, c *daemonconn.Connection, name, role string) (api.Party, error) {
	var party api.Party
	if id, err := uuid.Parse(name); err == nil && id.String() == name {
		party, err = c.Party(ctx, name)
		if err != nil {
			return api.Party{}, err
		}
	} else {
		for offset := 0; ; offset += 1000 {
			page, err := c.Parties(ctx, "", nil, false, 1000, offset)
			if err != nil {
				return api.Party{}, err
			}
			for _, candidate := range page.Items {
				if document.FoldPersonName(candidate.DisplayName) == document.FoldPersonName(name) {
					party = candidate
					break
				}
			}
			if party.PartyID != "" || len(page.Items) < 1000 {
				break
			}
		}
		if party.PartyID == "" {
			return api.Party{}, fmt.Errorf("party %q not found", name)
		}
	}
	if party.RetiredAt != nil {
		return api.Party{}, fmt.Errorf("party %q is retired", name)
	}
	if role == "own" && !party.Own {
		return api.Party{}, fmt.Errorf("party %q is not an own entity", name)
	}
	return party, nil
}

func newContractListCommand() *cobra.Command {
	var status, kind, party, expires, q, asOf string
	var limit, offset int
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "List contracts", Example: "docbank contract list --json", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.Contracts(cmd.Context(), status, kind, party, expires, q, asOf, limit, offset)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, v := range page.Items {
			if err = printContract(cmd, v, false); err != nil {
				return err
			}
		}
		return writeContinuation(cmd.OutOrStdout(), "contracts", page.Limit, page.Offset, len(page.Items))
	}}
	f := cmd.Flags()
	f.StringVar(&status, "status", "", "effective status")
	f.StringVar(&kind, "type", "", "contract type")
	f.StringVar(&party, "party-id", "", "party ID")
	f.StringVar(&expires, "expiring-before", "", "exclusive expiry date; combine with --status active for active renewals")
	f.StringVar(&q, "q", "", "literal title substring")
	f.StringVar(&asOf, "as-of", "", "civil date")
	f.IntVar(&limit, "limit", 100, "maximum results")
	f.IntVar(&offset, "offset", 0, "results to skip")
	f.BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}
func newContractShowCommand() *cobra.Command {
	var jsonOutput bool
	var asOf string
	cmd := &cobra.Command{Use: "show <contract-id>", Short: "Show a contract", Example: "docbank contract show 00000000-0000-4000-8000-000000000001", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		v, err := c.Contract(cmd.Context(), args[0], asOf)
		if err != nil {
			return err
		}
		return printContractShow(cmd, v, jsonOutput)
	}}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	cmd.Flags().StringVar(&asOf, "as-of", "", "civil date")
	return cmd
}
func newContractCreateCommand() *cobra.Command {
	var b api.CreateContractRequest
	var starts, expires, signed string
	var own, other []string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "create <title>", Short: "Create a contract", Example: "docbank contract create 'Service agreement'", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		b.Title = args[0]
		b.Parties = nil
		f := cmd.Flags()
		if f.Changed("status") && b.Status == "" {
			return errors.New("status cannot be empty")
		}
		if f.Changed("starts") {
			b.StartsOn = &starts
		}
		if f.Changed("expires") {
			b.ExpiresOn = &expires
		}
		if f.Changed("signed") {
			b.SignedOn = &signed
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		for _, group := range []struct {
			role  string
			names []string
		}{{"own", own}, {"counterparty", other}} {
			for _, name := range group.names {
				party, err := resolveContractParty(cmd.Context(), c, name, group.role)
				if err != nil {
					return err
				}
				b.Parties = append(b.Parties, api.ContractPartyInput{PartyID: party.PartyID, Role: group.role})
			}
		}
		v, err := c.CreateContract(cmd.Context(), b)
		if err != nil {
			return err
		}
		return printContract(cmd, v, jsonOutput)
	}}
	f := cmd.Flags()
	f.StringVar(&b.ContractType, "type", "", "contract type")
	f.StringVar(&b.Status, "status", "", "stored status")
	f.StringVar(&starts, "starts", "", "start date")
	f.StringVar(&expires, "expires", "", "expiry date")
	f.StringVar(&signed, "signed", "", "signing date")
	f.StringVar(&b.Summary, "summary", "", "summary")
	f.StringVar(&b.Purpose, "purpose", "", "purpose")
	f.StringVar(&b.Consideration, "consideration", "", "consideration")
	f.StringVar(&b.Termination, "termination", "", "termination")
	f.StringArrayVar(&own, "own", nil, "own party ID or name")
	f.StringArrayVar(&other, "counterparty", nil, "counterparty ID or name")
	f.BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}
func newContractEditCommand() *cobra.Command {
	var title, kind, status, starts, expires, signed, summary, purpose, consideration, termination string
	var clearStarts, clearExpires, clearSigned, jsonOutput bool
	var revision int64
	var own, other, removeOwn, removeOther []string
	cmd := &cobra.Command{Use: "edit <contract-id>", Short: "Edit a contract", Example: "docbank contract edit 00000000-0000-4000-8000-000000000001 --title 'Updated agreement'", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		if clearStarts && f.Changed("starts") || clearExpires && f.Changed("expires") || clearSigned && f.Changed("signed") {
			return errors.New("cannot set and clear the same date")
		}
		if f.Changed("revision") && revision < 1 {
			return errors.New("revision must be positive")
		}
		b := api.PatchContractRequest{ClearStarts: clearStarts, ClearExpires: clearExpires, ClearSigned: clearSigned}
		if f.Changed("title") {
			b.Title = &title
		}
		if f.Changed("type") {
			b.ContractType = &kind
		}
		if f.Changed("status") {
			b.Status = &status
		}
		if f.Changed("starts") {
			b.StartsOn = &starts
		}
		if f.Changed("expires") {
			b.ExpiresOn = &expires
		}
		if f.Changed("signed") {
			b.SignedOn = &signed
		}
		if f.Changed("summary") {
			b.Summary = &summary
		}
		if f.Changed("purpose") {
			b.Purpose = &purpose
		}
		if f.Changed("consideration") {
			b.Consideration = &consideration
		}
		if f.Changed("termination") {
			b.Termination = &termination
		}
		hasFields := b.Title != nil || b.ContractType != nil || b.Status != nil || b.StartsOn != nil || b.ExpiresOn != nil || b.SignedOn != nil || b.Summary != nil || b.Purpose != nil || b.Consideration != nil || b.Termination != nil || clearStarts || clearExpires || clearSigned
		if !hasFields && len(own)+len(other)+len(removeOwn)+len(removeOther) == 0 {
			return fmt.Errorf("no changes requested")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		v, err := c.Contract(cmd.Context(), args[0], "")
		if err != nil {
			return err
		}
		expected := revision
		if expected == 0 {
			expected = v.Revision
		}
		type change struct {
			role, name, id string
			attach         bool
		}
		var changes []change
		for _, group := range []struct {
			role   string
			names  []string
			attach bool
		}{{"own", own, true}, {"counterparty", other, true}, {"own", removeOwn, false}, {"counterparty", removeOther, false}} {
			for _, name := range group.names {
				var id string
				var party api.Party
				if group.attach {
					party, err = resolveContractParty(cmd.Context(), c, name, group.role)
					id = party.PartyID
				} else {
					id, err = resolveLinkedContractParty(v, group.role, name)
				}
				if err != nil {
					return err
				}
				changes = append(changes, change{group.role, name, id, group.attach})
				// Track the planned link state so later removals can refer to earlier attaches.
				linked := false
				for i := 0; i < len(v.Parties); i++ {
					if v.Parties[i].PartyID == id && v.Parties[i].Role == group.role {
						linked = true
						if !group.attach {
							v.Parties = append(v.Parties[:i], v.Parties[i+1:]...)
						}
						break
					}
				}
				if group.attach && !linked {
					v.Parties = append(v.Parties, api.ContractParty{PartyID: id, Role: group.role, DisplayName: party.DisplayName})
				}
			}
		}
		var applied []string
		if hasFields {
			v, err = c.UpdateContract(cmd.Context(), args[0], expected, b)
			if err != nil {
				return err
			}
			expected = v.Revision
			applied = append(applied, "contract fields")
		}
		for _, change := range changes {
			if change.attach {
				v, err = c.AttachContractParty(cmd.Context(), args[0], expected, change.id, change.role)
			} else {
				v, err = c.DetachContractParty(cmd.Context(), args[0], expected, change.id, change.role)
			}
			if err != nil {
				return fmt.Errorf("applied %v; %w", applied, err)
			}
			expected = v.Revision
			applied = append(applied, change.role+":"+change.name)
		}
		return printContract(cmd, v, jsonOutput)
	}}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "title")
	f.StringVar(&kind, "type", "", "type")
	f.StringVar(&status, "status", "", "stored status")
	f.StringVar(&starts, "starts", "", "start date")
	f.StringVar(&expires, "expires", "", "expiry date")
	f.StringVar(&signed, "signed", "", "signing date")
	f.BoolVar(&clearStarts, "clear-starts", false, "clear start date")
	f.BoolVar(&clearExpires, "clear-expires", false, "clear expiry date")
	f.BoolVar(&clearSigned, "clear-signed", false, "clear signing date")
	f.StringVar(&summary, "summary", "", "summary")
	f.StringVar(&purpose, "purpose", "", "purpose")
	f.StringVar(&consideration, "consideration", "", "consideration")
	f.StringVar(&termination, "termination", "", "termination")
	f.StringArrayVar(&own, "own", nil, "attach own party")
	f.StringArrayVar(&other, "counterparty", nil, "attach counterparty")
	f.StringArrayVar(&removeOwn, "remove-own", nil, "detach own party")
	f.StringArrayVar(&removeOther, "remove-counterparty", nil, "detach counterparty")
	f.Int64Var(&revision, "revision", 0, "expected revision")
	f.BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}
func newContractDeleteCommand() *cobra.Command {
	var revision int64
	var jsonOutput bool
	cmd := &cobra.Command{Use: "delete <contract-id>", Short: "Delete a contract", Example: "docbank contract delete 00000000-0000-4000-8000-000000000001", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("revision") && revision < 1 {
			return errors.New("revision must be positive")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if revision == 0 {
			v, err := c.Contract(cmd.Context(), args[0], "")
			if err != nil {
				return err
			}
			revision = v.Revision
		}
		if err = c.DeleteContract(cmd.Context(), args[0], revision); err != nil {
			return err
		}
		if jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), map[string]string{"deleted": args[0]})
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Deleted", args[0])
		if err != nil {
			return fmt.Errorf("write deletion result: %w", err)
		}
		return nil
	}}
	cmd.Flags().Int64Var(&revision, "revision", 0, "expected revision")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}
func init() {
	contractCmd.AddCommand(newContractListCommand(), newContractShowCommand(), newContractCreateCommand(), newContractEditCommand(), newContractDeleteCommand())
}
