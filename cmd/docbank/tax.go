package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newTaxCommand() *cobra.Command {
	root := &cobra.Command{Use: "tax", Short: "Manage tax accounts", Long: "Manage tax accounts and their registration details.\nAccount identifiers are masked unless explicitly revealed.", GroupID: groupDocuments}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		message := "invalid tax command flags"
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(err.Error(), `for "--`+f.Name+`" flag`) || strings.Contains(err.Error(), "flag needs an argument: --"+f.Name) {
				message = "invalid value for --" + f.Name
			}
		})
		return usageError(errors.New(message))
	})
	root.Args = taxNoArgs
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	root.PersistentFlags().Bool("json", false, "print JSON")
	root.AddCommand(newTaxAccountCommand())
	return root
}

func taxNoArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usageError(errors.New("unexpected tax command arguments"))
	}
	return nil
}

func newTaxAccountCommand() *cobra.Command {
	account := &cobra.Command{Use: "account", Short: "Manage tax accounts", Long: "Create, inspect, edit, list, and retire tax accounts.\nEdits and retirement use the current revision unless one is supplied."}
	account.Args = taxNoArgs
	account.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	printAccount := func(cmd *cobra.Command, a api.TaxAccount) error {
		if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), a)
		}
		value := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\tfiler: %s\tauthority: %s\tcountry: %s\tsubdivision: %s\tkind: %q\tidentifier: %q\tcadence: %s\tperiod: %q\tdue: %q\tlead days: %d\tcurrency: %s\tstart: %s\tend: %s\tnote: %q\tretired: %s\trevision %d\tcreated: %s\tupdated: %s\n", a.AccountID, a.FilerPartyID, value(a.AuthorityPartyID), a.Country, a.Subdivision, a.TaxKind, a.Identifier, a.Cadence, a.PeriodNote, a.DueNote, a.LeadDays, a.Currency, value(a.EffectiveStart), value(a.EffectiveEnd), a.Note, value(a.RetiredAt), a.Revision, a.CreatedAt, a.UpdatedAt)
		return err
	}
	var filters api.TaxAccountListOptions
	list := &cobra.Command{Use: "list", Short: "List tax accounts", Example: "docbank tax account list --country ZZ", Args: taxNoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.TaxAccounts(cmd.Context(), filters)
		if err != nil {
			return err
		}
		if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, a := range page.Items {
			if err := printAccount(cmd, a); err != nil {
				return err
			}
		}
		if page.Limit > 0 && len(page.Items) == page.Limit {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "limit reached: more accounts may exist; repeat this command with --offset %d to continue\n", page.Offset+len(page.Items))
			return err
		}
		return nil
	}}
	list.Flags().StringVar(&filters.FilerPartyID, "filer", "", "filter by filer party ID")
	list.Flags().StringVar(&filters.Country, "country", "", "filter by country")
	list.Flags().StringVar(&filters.TaxKind, "taxkind", "", "filter by tax kind")
	list.Flags().BoolVar(&filters.IncludeRetired, "include-retired", false, "include retired accounts")
	list.Flags().IntVar(&filters.Limit, "limit", 100, "maximum number of accounts")
	list.Flags().IntVar(&filters.Offset, "offset", 0, "number of accounts to skip")
	var reveal bool
	show := &cobra.Command{Use: "show <account-id>", Short: "Inspect a tax account", Example: "docbank tax account show 11111111-1111-4111-8111-111111111111", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		a, err := c.TaxAccount(cmd.Context(), args[0], reveal)
		if err != nil {
			return err
		}
		return printAccount(cmd, a)
	}}
	show.Flags().BoolVar(&reveal, "reveal", false, "show the complete account identifier")
	var add api.CreateTaxAccountRequest
	var authority, start, end string
	var lead int64
	create := &cobra.Command{Use: "add", Short: "Create a tax account", Example: "docbank tax account add --filer 11111111-1111-4111-8111-111111111111 --country ZZ --tax-kind Example --cadence annual --currency USD", Args: taxNoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("authority") {
			add.AuthorityPartyID = &authority
		}
		if cmd.Flags().Changed("effective-start") {
			add.EffectiveStart = &start
		}
		if cmd.Flags().Changed("effective-end") {
			add.EffectiveEnd = &end
		}
		if cmd.Flags().Changed("lead-days") {
			add.LeadDays = &lead
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		a, err := c.CreateTaxAccount(cmd.Context(), add)
		if err != nil {
			return err
		}
		return printAccount(cmd, a)
	}}
	create.Flags().StringVar(&add.FilerPartyID, "filer", "", "filer party ID")
	create.Flags().StringVar(&authority, "authority", "", "authority party ID")
	create.Flags().StringVar(&add.Country, "country", "", "country code")
	create.Flags().StringVar(&add.Subdivision, "subdivision", "", "subdivision code")
	create.Flags().StringVar(&add.TaxKind, "tax-kind", "", "owner-chosen tax kind")
	create.Flags().StringVar(&add.Identifier, "identifier", "", "account identifier")
	create.Flags().StringVar(&add.Cadence, "cadence", "", "filing cadence")
	create.Flags().StringVar(&add.PeriodNote, "period-note", "", "period note")
	create.Flags().StringVar(&add.DueNote, "due-note", "", "due date note")
	create.Flags().Int64Var(&lead, "lead-days", 0, "lead days")
	create.Flags().StringVar(&add.Currency, "currency", "", "default currency")
	create.Flags().StringVar(&start, "effective-start", "", "effective start date")
	create.Flags().StringVar(&end, "effective-end", "", "effective end date")
	create.Flags().StringVar(&add.Note, "note", "", "account note")
	for _, name := range []string{"filer", "country", "tax-kind", "cadence", "currency"} {
		_ = create.MarkFlagRequired(name)
	}
	var editIdentifier, editCadence, periodNote, dueNote, currency, editStart, editEnd, note string
	var editLead, revision int64
	var clearStart, clearEnd bool
	edit := &cobra.Command{Use: "edit <account-id>", Short: "Edit a tax account", Long: "Edit account details with a revision fence.\nWithout --revision, inspect once and send one fenced update.\nStale updates are not retried.", Example: "docbank tax account edit 11111111-1111-4111-8111-111111111111 --lead-days 7", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		in := api.PatchTaxAccountRequest{ClearEffectiveStart: clearStart, ClearEffectiveEnd: clearEnd}
		if f.Changed("identifier") {
			in.Identifier = &editIdentifier
		}
		if f.Changed("cadence") {
			in.Cadence = &editCadence
		}
		if f.Changed("period-note") {
			in.PeriodNote = &periodNote
		}
		if f.Changed("due-note") {
			in.DueNote = &dueNote
		}
		if f.Changed("currency") {
			in.Currency = &currency
		}
		if f.Changed("note") {
			in.Note = &note
		}
		if f.Changed("lead-days") {
			in.LeadDays = &editLead
		}
		if f.Changed("effective-start") {
			in.EffectiveStart = &editStart
		}
		if f.Changed("effective-end") {
			in.EffectiveEnd = &editEnd
		}
		if clearStart && in.EffectiveStart != nil || clearEnd && in.EffectiveEnd != nil {
			return usageError(errors.New("cannot set and clear the same field"))
		}
		if in.Identifier == nil && in.Cadence == nil && in.PeriodNote == nil && in.DueNote == nil && in.Currency == nil && in.Note == nil && in.LeadDays == nil && in.EffectiveStart == nil && in.EffectiveEnd == nil && !clearStart && !clearEnd {
			return usageError(errors.New("provide at least one change"))
		}
		if f.Changed("revision") && revision < 1 {
			return usageError(errors.New("revision must be positive"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rev := revision
		if !f.Changed("revision") {
			a, err := c.TaxAccount(cmd.Context(), args[0], false)
			if err != nil {
				return err
			}
			rev = a.Revision
		}
		a, err := c.UpdateTaxAccount(cmd.Context(), args[0], rev, in)
		if err != nil {
			return err
		}
		return printAccount(cmd, a)
	}}
	edit.Flags().StringVar(&editIdentifier, "identifier", "", "account identifier")
	edit.Flags().StringVar(&editCadence, "cadence", "", "filing cadence")
	edit.Flags().StringVar(&periodNote, "period-note", "", "period note")
	edit.Flags().StringVar(&dueNote, "due-note", "", "due date note")
	edit.Flags().StringVar(&currency, "currency", "", "default currency")
	edit.Flags().StringVar(&editStart, "effective-start", "", "effective start date")
	edit.Flags().StringVar(&editEnd, "effective-end", "", "effective end date")
	edit.Flags().StringVar(&note, "note", "", "account note")
	edit.Flags().Int64Var(&editLead, "lead-days", 0, "lead days")
	edit.Flags().BoolVar(&clearStart, "clear-effective-start", false, "remove effective start date")
	edit.Flags().BoolVar(&clearEnd, "clear-effective-end", false, "remove effective end date")
	edit.Flags().Int64Var(&revision, "revision", 0, "expected revision (default: current)")
	var retireRevision int64
	retire := &cobra.Command{Use: "retire <account-id>", Short: "Retire a tax account", Long: "Retire an account with a revision fence.\nWithout --revision, inspect once and send one fenced update.\nStale updates are not retried.", Example: "docbank tax account retire 11111111-1111-4111-8111-111111111111", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("revision") && retireRevision < 1 {
			return usageError(errors.New("revision must be positive"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rev := retireRevision
		if !cmd.Flags().Changed("revision") {
			a, err := c.TaxAccount(cmd.Context(), args[0], false)
			if err != nil {
				return err
			}
			rev = a.Revision
		}
		a, err := c.RetireTaxAccount(cmd.Context(), args[0], rev)
		if err != nil {
			return err
		}
		return printAccount(cmd, a)
	}}
	retire.Flags().Int64Var(&retireRevision, "revision", 0, "expected revision (default: current)")
	account.AddCommand(create, edit, list, show, retire)
	return account
}
func init() { rootCmd.AddCommand(newTaxCommand()) }
