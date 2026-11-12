package main

import (
	"context"
	"fmt"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func printContract(cmd *cobra.Command, c api.Contract, jsonOutput bool) error {
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), c)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\trevision %d\n", c.ContractID, c.Title, c.EffectiveStatus, c.Revision)
	return err
}
func resolveContractParty(ctx context.Context, c *daemonconn.Connection, name string) (string, error) {
	// A canonical ID may be used directly; names are matched against active parties.
	if id, err := uuid.Parse(name); err == nil && id.String() == name {
		return name, nil
	}
	for offset := 0; ; offset += 1000 {
		page, err := c.Parties(ctx, "", nil, false, 1000, offset)
		if err != nil {
			return "", err
		}
		for _, p := range page.Items {
			if document.FoldPersonName(p.DisplayName) == document.FoldPersonName(name) {
				return p.PartyID, nil
			}
		}
		if len(page.Items) < 1000 {
			break
		}
	}
	return "", fmt.Errorf("party %q not found", name)
}
func newContractListCommand() *cobra.Command {
	var status, kind, party, expires, q, asOf string
	var limit, offset int
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "List contracts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
		return nil
	}}
	f := cmd.Flags()
	f.StringVar(&status, "status", "", "Effective status")
	f.StringVar(&kind, "type", "", "Contract type")
	f.StringVar(&party, "party-id", "", "Party ID")
	f.StringVar(&expires, "expiring-before", "", "Exclusive expiry date")
	f.StringVar(&q, "q", "", "Literal title substring")
	f.StringVar(&asOf, "as-of", "", "Civil date")
	f.IntVar(&limit, "limit", 100, "Maximum results")
	f.IntVar(&offset, "offset", 0, "Results to skip")
	f.BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}
func newContractShowCommand() *cobra.Command {
	var jsonOutput bool
	var asOf string
	cmd := &cobra.Command{Use: "show <contract-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		v, err := c.Contract(cmd.Context(), args[0], asOf)
		if err != nil {
			return err
		}
		return printContract(cmd, v, jsonOutput)
	}}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	cmd.Flags().StringVar(&asOf, "as-of", "", "Civil date")
	return cmd
}
func newContractCreateCommand() *cobra.Command {
	var b api.CreateContractRequest
	var starts, expires, signed string
	var own, other []string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "create <title>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		b.Title = args[0]
		f := cmd.Flags()
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
				id, err := resolveContractParty(cmd.Context(), c, name)
				if err != nil {
					return err
				}
				b.Parties = append(b.Parties, api.ContractPartyInput{PartyID: id, Role: group.role})
			}
		}
		v, err := c.CreateContract(cmd.Context(), b)
		if err != nil {
			return err
		}
		return printContract(cmd, v, jsonOutput)
	}}
	f := cmd.Flags()
	f.StringVar(&b.ContractType, "type", "", "Contract type")
	f.StringVar(&b.Status, "status", "", "Stored status")
	f.StringVar(&starts, "starts", "", "Start date")
	f.StringVar(&expires, "expires", "", "Expiry date")
	f.StringVar(&signed, "signed", "", "Signing date")
	f.StringVar(&b.Summary, "summary", "", "Summary")
	f.StringVar(&b.Purpose, "purpose", "", "Purpose")
	f.StringVar(&b.Consideration, "consideration", "", "Consideration")
	f.StringVar(&b.Termination, "termination", "", "Termination")
	f.StringArrayVar(&own, "own", nil, "Own party ID or name")
	f.StringArrayVar(&other, "counterparty", nil, "Counterparty ID or name")
	f.BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}
func newContractEditCommand() *cobra.Command {
	var title, kind, status, starts, expires, signed, summary, purpose, consideration, termination string
	var clearStarts, clearExpires, clearSigned, jsonOutput bool
	var revision int64
	var own, other, removeOwn, removeOther []string
	cmd := &cobra.Command{Use: "edit <contract-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		if clearStarts && f.Changed("starts") || clearExpires && f.Changed("expires") || clearSigned && f.Changed("signed") {
			return fmt.Errorf("cannot set and clear the same date")
		}
		if f.Changed("revision") && revision < 1 {
			return fmt.Errorf("revision must be positive")
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
		v, err := c.UpdateContract(cmd.Context(), args[0], revision, b)
		if err != nil {
			return err
		}
		applied := []string{"contract fields"}
		for _, group := range []struct {
			role   string
			names  []string
			attach bool
		}{{"own", own, true}, {"counterparty", other, true}, {"own", removeOwn, false}, {"counterparty", removeOther, false}} {
			for _, name := range group.names {
				id, e := resolveContractParty(cmd.Context(), c, name)
				if e != nil {
					return fmt.Errorf("applied %v; %w", applied, e)
				}
				if group.attach {
					v, e = c.AttachContractParty(cmd.Context(), args[0], v.Revision, id, group.role)
				} else {
					v, e = c.DetachContractParty(cmd.Context(), args[0], v.Revision, id, group.role)
				}
				if e != nil {
					return fmt.Errorf("applied %v; %w", applied, e)
				}
				applied = append(applied, group.role+":"+name)
			}
		}
		return printContract(cmd, v, jsonOutput)
	}}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "Title")
	f.StringVar(&kind, "type", "", "Type")
	f.StringVar(&status, "status", "", "Stored status")
	f.StringVar(&starts, "starts", "", "Start date")
	f.StringVar(&expires, "expires", "", "Expiry date")
	f.StringVar(&signed, "signed", "", "Signing date")
	f.BoolVar(&clearStarts, "clear-starts", false, "Clear start date")
	f.BoolVar(&clearExpires, "clear-expires", false, "Clear expiry date")
	f.BoolVar(&clearSigned, "clear-signed", false, "Clear signing date")
	f.StringVar(&summary, "summary", "", "Summary")
	f.StringVar(&purpose, "purpose", "", "Purpose")
	f.StringVar(&consideration, "consideration", "", "Consideration")
	f.StringVar(&termination, "termination", "", "Termination")
	f.StringArrayVar(&own, "own", nil, "Attach own party")
	f.StringArrayVar(&other, "counterparty", nil, "Attach counterparty")
	f.StringArrayVar(&removeOwn, "remove-own", nil, "Detach own party")
	f.StringArrayVar(&removeOther, "remove-counterparty", nil, "Detach counterparty")
	f.Int64Var(&revision, "revision", 0, "Expected revision")
	f.BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}
func newContractDeleteCommand() *cobra.Command {
	var revision int64
	var jsonOutput bool
	cmd := &cobra.Command{Use: "delete <contract-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("revision") && revision < 1 {
			return fmt.Errorf("revision must be positive")
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
		return err
	}}
	cmd.Flags().Int64Var(&revision, "revision", 0, "Expected revision")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}
func init() {
	contractCmd.AddCommand(newContractListCommand(), newContractShowCommand(), newContractCreateCommand(), newContractEditCommand(), newContractDeleteCommand())
}
