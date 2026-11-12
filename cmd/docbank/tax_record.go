package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/taxrecord"
)

type recordFlags struct {
	account, filer, authority, kind, title, country, subdivision, taxKind, form, periodStart, periodEnd, due, direction, amount, currency, note string
	year, lead                                                                                                                                  int64
	clearAuthority, clearYear, clearStart, clearEnd, clearDue, clearLead, clearAmount                                                           bool
}

func (f *recordFlags) bind(cmd *cobra.Command, create bool) {
	flags := cmd.Flags()
	if create {
		flags.StringVar(&f.account, "account", "", "account ID")
		flags.StringVar(&f.kind, "kind", "", "record kind: "+strings.Join(taxrecord.RecordKinds(), ", "))
	}
	flags.StringVar(&f.filer, "filer", "", "filer party ID")
	flags.StringVar(&f.authority, "authority", "", "authority party ID")
	flags.StringVar(&f.title, "title", "", "record title")
	flags.StringVar(&f.country, "country", "", "country code")
	flags.StringVar(&f.subdivision, "subdivision", "", "subdivision code")
	flags.StringVar(&f.taxKind, "tax-kind", "", "owner-chosen tax kind")
	flags.StringVar(&f.form, "form-code", "", "form code")
	flags.Int64Var(&f.year, "tax-year", 0, "tax year")
	flags.StringVar(&f.periodStart, "period-start", "", "period start date")
	flags.StringVar(&f.periodEnd, "period-end", "", "period end date")
	flags.StringVar(&f.due, "due-date", "", "due date")
	flags.Int64Var(&f.lead, "lead-days", 0, "lead days")
	flags.StringVar(&f.direction, "direction", "", "amount direction: "+strings.Join(taxrecord.RecordDirections(), ", "))
	flags.StringVar(&f.amount, "amount", "", "exact decimal amount; direction must be payable, refundable, or nil")
	flags.StringVar(&f.currency, "currency", "", "currency code")
	flags.StringVar(&f.note, "note", "", "record note")
	if !create {
		flags.BoolVar(&f.clearAuthority, "clear-authority", false, "clear authority")
		flags.BoolVar(&f.clearYear, "clear-tax-year", false, "clear tax year")
		flags.BoolVar(&f.clearStart, "clear-period-start", false, "clear period start")
		flags.BoolVar(&f.clearEnd, "clear-period-end", false, "clear period end")
		flags.BoolVar(&f.clearDue, "clear-due-date", false, "clear due date")
		flags.BoolVar(&f.clearLead, "clear-lead-days", false, "clear lead days")
		flags.BoolVar(&f.clearAmount, "clear-amount", false, "clear amount")
	}
}
func (f *recordFlags) patch(cmd *cobra.Command) (api.PatchTaxRecordRequest, error) {
	flags := cmd.Flags()
	p := api.PatchTaxRecordRequest{ClearAuthority: f.clearAuthority, ClearTaxYear: f.clearYear, ClearPeriodStart: f.clearStart, ClearPeriodEnd: f.clearEnd, ClearDueDate: f.clearDue, ClearLeadDays: f.clearLead, ClearAmount: f.clearAmount}
	for _, v := range []struct {
		name  string
		clear bool
	}{{"authority", f.clearAuthority}, {"tax-year", f.clearYear}, {"period-start", f.clearStart}, {"period-end", f.clearEnd}, {"due-date", f.clearDue}, {"lead-days", f.clearLead}, {"amount", f.clearAmount}} {
		if v.clear && flags.Changed(v.name) {
			return p, usageError(errors.New("cannot set and clear the same field"))
		}
	}
	if flags.Changed("filer") {
		p.FilerPartyID = &f.filer
	}
	if flags.Changed("authority") {
		p.AuthorityPartyID = &f.authority
	}
	if flags.Changed("title") {
		p.Title = &f.title
	}
	if flags.Changed("country") {
		p.Country = &f.country
	}
	if flags.Changed("subdivision") {
		p.Subdivision = &f.subdivision
	}
	if flags.Changed("tax-kind") {
		p.TaxKind = &f.taxKind
	}
	if flags.Changed("form-code") {
		p.FormCode = &f.form
	}
	if flags.Changed("tax-year") {
		p.TaxYear = &f.year
	}
	if flags.Changed("period-start") {
		p.PeriodStart = &f.periodStart
	}
	if flags.Changed("period-end") {
		p.PeriodEnd = &f.periodEnd
	}
	if flags.Changed("due-date") {
		p.DueDate = &f.due
	}
	if flags.Changed("lead-days") {
		p.LeadDays = &f.lead
	}
	if flags.Changed("direction") {
		p.Direction = &f.direction
	}
	if flags.Changed("amount") {
		p.Amount = &f.amount
	}
	if flags.Changed("currency") {
		p.Currency = &f.currency
	}
	if flags.Changed("note") {
		p.Note = &f.note
	}
	changed := f.clearAuthority || f.clearYear || f.clearStart || f.clearEnd || f.clearDue || f.clearLead || f.clearAmount
	for _, name := range []string{"filer", "authority", "title", "country", "subdivision", "tax-kind", "form-code", "tax-year", "period-start", "period-end", "due-date", "lead-days", "direction", "amount", "currency", "note"} {
		changed = changed || flags.Changed(name)
	}
	if !changed {
		return p, usageError(errors.New("provide at least one change"))
	}
	return p, nil
}
func (f *recordFlags) create(cmd *cobra.Command) (api.CreateTaxRecordRequest, error) {
	if cmd.Flags().Changed("direction") && f.direction == "" {
		return api.CreateTaxRecordRequest{}, usageError(errors.New("direction: unknown direction"))
	}
	in := api.CreateTaxRecordRequest{Kind: f.kind, Title: f.title, FormCode: f.form, Direction: f.direction, Note: f.note}
	flags := cmd.Flags()
	if flags.Changed("account") {
		in.AccountID = &f.account
	}
	if flags.Changed("filer") {
		in.FilerPartyID = &f.filer
	}
	if flags.Changed("authority") {
		in.AuthorityPartyID = &f.authority
	}
	if flags.Changed("country") {
		in.Country = &f.country
	}
	if flags.Changed("subdivision") {
		in.Subdivision = &f.subdivision
	}
	if flags.Changed("tax-kind") {
		in.TaxKind = &f.taxKind
	}
	if flags.Changed("tax-year") {
		in.TaxYear = &f.year
	}
	if flags.Changed("period-start") {
		in.PeriodStart = &f.periodStart
	}
	if flags.Changed("period-end") {
		in.PeriodEnd = &f.periodEnd
	}
	if flags.Changed("due-date") {
		in.DueDate = &f.due
	}
	if flags.Changed("lead-days") {
		in.LeadDays = &f.lead
	}
	if flags.Changed("amount") {
		in.Amount = &f.amount
	}
	if flags.Changed("currency") {
		in.Currency = &f.currency
	}
	return in, nil
}
func printTaxRecord(cmd *cobra.Command, r api.TaxRecord) error {
	if v, _ := cmd.Flags().GetBool("json"); v {
		return writeCLIJSON(cmd.OutOrStdout(), r)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\taccount: %v\tfiler: %s\tauthority: %v\tkind: %s\ttitle: %q\tcountry: %s\tsubdivision: %s\ttax kind: %q\tform: %q\tyear: %v\tperiod start: %v\tperiod end: %v\tdue: %v\tlead days: %v\tstate: %s\tdirection: %s\tamount minor: %v\tcurrency: %s\tverification: %s\tverified on: %v\tverified revision: %v\tverification note: %q\tnote: %q\tretired: %v\trevision %d\tcreated: %s\tupdated: %s\n", r.RecordID, taxValue(r.AccountID), r.FilerPartyID, taxValue(r.AuthorityPartyID), r.Kind, r.Title, r.Country, r.Subdivision, r.TaxKind, r.FormCode, taxValue(r.TaxYear), taxValue(r.PeriodStart), taxValue(r.PeriodEnd), taxValue(r.DueDate), taxValue(r.LeadDays), r.State, r.Direction, taxValue(r.AmountMinor), r.Currency, r.Verification, taxValue(r.VerifiedOn), taxValue(r.VerifiedRevision), r.VerificationNote, r.Note, taxValue(r.RetiredAt), r.Revision, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("print tax record: %w", err)
	}
	return nil
}
func taxValue[T any](p *T) any {
	if p == nil {
		return ""
	}
	return *p
}
func taxRecordID(id string) error {
	if canonical.ValidateUUIDv4(id) != nil {
		return usageError(errors.New("invalid record ID"))
	}
	return nil
}
func newTaxRecordCommand() *cobra.Command {
	group := &cobra.Command{Use: "record", Short: "Manage tax records", Long: "Create, inspect, edit, list, and retire initial tax records.\nEdits and retirement inspect the current revision unless one is supplied.", Args: taxGroupArgs, Annotations: map[string]string{helpOnlyAnnotation: helpOnlyMarker}, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var filer, country, taxkind, state, kind string
	var year int64
	var include bool
	var limit, offset int
	list := &cobra.Command{Use: "list", Short: "List tax records", Example: "docbank tax record list --country ZZ", Args: taxNoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 1000 || offset < 0 {
			return usageError(errors.New("invalid page bounds"))
		}
		var y *int64
		if cmd.Flags().Changed("year") {
			if year < 1 || year > 9999 {
				return usageError(errors.New("year must be between 1 and 9999"))
			}
			y = &year
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.TaxRecords(cmd.Context(), filer, country, taxkind, y, state, kind, include, limit, offset)
		if err != nil {
			return err
		}
		if v, _ := cmd.Flags().GetBool("json"); v {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, r := range page.Items {
			if err := printTaxRecord(cmd, r); err != nil {
				return err
			}
		}
		if len(page.Items) == page.Limit {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "limit reached: more records may exist; repeat this command with --offset %d to continue\n", page.Offset+len(page.Items)); err != nil {
				return fmt.Errorf("print tax record continuation: %w", err)
			}
		}
		return nil
	}}
	list.Flags().StringVar(&filer, "filer", "", "filter by filer party ID")
	list.Flags().StringVar(&country, "country", "", "filter by country")
	list.Flags().StringVar(&taxkind, "taxkind", "", "filter by tax kind")
	list.Flags().Int64Var(&year, "year", 0, "filter by tax year")
	list.Flags().StringVar(&state, "state", "", "filter by state: "+strings.Join(taxrecord.RecordStates(), ", "))
	list.Flags().StringVar(&kind, "kind", "", "filter by record kind: "+strings.Join(taxrecord.RecordKinds(), ", "))
	list.Flags().BoolVar(&include, "include-retired", false, "include retired records")
	list.Flags().IntVar(&limit, "limit", 100, "maximum number of records")
	list.Flags().IntVar(&offset, "offset", 0, "number of records to skip")
	show := &cobra.Command{Use: "show <record-id>", Short: "Inspect a tax record", Example: "docbank tax record show 11111111-1111-4111-8111-111111111111", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := taxRecordID(args[0]); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		reveal, _ := cmd.Flags().GetBool("reveal")
		r, err := c.TaxRecord(cmd.Context(), args[0], reveal)
		if err != nil {
			return err
		}
		if v, _ := cmd.Flags().GetBool("json"); !v {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "outstanding minor: %v\tcurrency: %s\tother currency events: %d\n", taxValue(r.Balance.OutstandingMinor), r.Balance.Currency, r.Balance.OtherCurrencyEvents); err != nil {
				return err
			}
		}
		return printTaxRecordDetail(cmd, r)
	}}
	show.Flags().Bool("reveal", false, "reveal event references")
	var add recordFlags
	create := &cobra.Command{Use: "add", Short: "Create a tax record", Example: "docbank tax record add --account 11111111-1111-4111-8111-111111111111 --kind filing --title Example", Args: taxNoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		in, err := add.create(cmd)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		r, err := c.CreateTaxRecord(cmd.Context(), in)
		if err != nil {
			return err
		}
		return printTaxRecord(cmd, r)
	}}
	add.bind(create, true)
	_ = create.MarkFlagRequired("kind")
	_ = create.MarkFlagRequired("title")
	var change recordFlags
	var editRev, retireRev int64
	edit := &cobra.Command{Use: "edit <record-id>", Short: "Edit a tax record", Long: "Edit a tax record.\nReads the current revision once unless --revision is supplied.\nStale updates are not retried.", Example: "docbank tax record edit 11111111-1111-4111-8111-111111111111 --note Example", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := taxRecordID(args[0]); err != nil {
			return err
		}
		p, err := change.patch(cmd)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("revision") && editRev < 1 {
			return usageError(errors.New("revision must be positive"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rev := editRev
		if !cmd.Flags().Changed("revision") {
			r, err := c.TaxRecord(cmd.Context(), args[0], false)
			if err != nil {
				return err
			}
			rev = r.Revision
		}
		r, err := c.UpdateTaxRecord(cmd.Context(), args[0], rev, p)
		if err != nil {
			return err
		}
		return printTaxRecord(cmd, r)
	}}
	change.bind(edit, false)
	edit.Flags().Int64Var(&editRev, "revision", 0, "expected revision (default: current)")
	retire := &cobra.Command{Use: "retire <record-id>", Short: "Retire a tax record", Long: "Retire a tax record.\nReads the current revision once unless --revision is supplied.\nStale updates are not retried.", Example: "docbank tax record retire 11111111-1111-4111-8111-111111111111", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := taxRecordID(args[0]); err != nil {
			return err
		}
		if cmd.Flags().Changed("revision") && retireRev < 1 {
			return usageError(errors.New("revision must be positive"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rev := retireRev
		if !cmd.Flags().Changed("revision") {
			r, err := c.TaxRecord(cmd.Context(), args[0], false)
			if err != nil {
				return err
			}
			rev = r.Revision
		}
		r, err := c.RetireTaxRecord(cmd.Context(), args[0], rev)
		if err != nil {
			return err
		}
		return printTaxRecord(cmd, r)
	}}
	retire.Flags().Int64Var(&retireRev, "revision", 0, "expected revision (default: current)")
	group.AddCommand(create, edit, list, show, retire, newTaxRecordAdvanceCommand())
	return group
}
