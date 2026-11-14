package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

func printTaxRecordDetail(cmd *cobra.Command, d api.TaxRecordDetail) error {
	if v, _ := cmd.Flags().GetBool("json"); v {
		return writeCLIJSON(cmd.OutOrStdout(), d)
	}
	if err := printTaxRecord(cmd, d.TaxRecord); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "outstanding minor: %v\tcurrency: %s\tother currency events: %d\n", taxValue(d.Balance.OutstandingMinor), d.Balance.Currency, d.Balance.OtherCurrencyEvents); err != nil {
		return fmt.Errorf("print tax balance: %w", err)
	}
	return printTaxEvents(cmd, d.Events)
}
func printTaxEvents(cmd *cobra.Command, events []api.TaxEvent) error {
	for _, e := range events {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "event %s\tkind: %s\toccurred: %s\tstate: %s\tamount minor: %v\tcurrency: %s\treference: %q\tnote: %q\tvoided: %v\treason: %q\trevision %d\n", e.EventID, e.Kind, e.OccurredOn, e.SetsState, taxValue(e.AmountMinor), e.Currency, e.Reference, e.Note, taxValue(e.VoidedAt), e.VoidReason, e.RecordRevision); err != nil {
			return fmt.Errorf("print tax event: %w", err)
		}
	}
	return nil
}
func printTaxEventMutation(cmd *cobra.Command, m api.TaxEventMutation) error {
	if v, _ := cmd.Flags().GetBool("json"); v {
		return writeCLIJSON(cmd.OutOrStdout(), m)
	}
	if err := printTaxRecord(cmd, m.Record); err != nil {
		return err
	}
	return printTaxEvents(cmd, []api.TaxEvent{m.Event})
}

type eventFlags struct {
	kind, date, amount, currency, reference, note string
	revision                                      int64
}

func (f *eventFlags) bind(cmd *cobra.Command, flag, usage string) {
	cmd.Flags().StringVar(&f.kind, flag, "", usage)
	cmd.Flags().StringVar(&f.date, "occurred-on", "", "event date")
	cmd.Flags().StringVar(&f.reference, "reference", "", "event reference")
	cmd.Flags().StringVar(&f.note, "note", "", "event note")
	cmd.Flags().Int64Var(&f.revision, "revision", 0, "expected revision (default: current)")
	_ = cmd.MarkFlagRequired(flag)
	_ = cmd.MarkFlagRequired("occurred-on")
}
func eventConnection(cmd *cobra.Command, id string, revision int64) (*daemonconn.Connection, int64, error) {
	if err := taxRecordID(id); err != nil {
		return nil, 0, err
	}
	if cmd.Flags().Changed("revision") && revision < 1 {
		return nil, 0, usageError(errors.New("revision must be positive"))
	}
	c, err := daemonconn.Ensure(cmd.Context())
	if err != nil {
		return nil, 0, err
	}
	if !cmd.Flags().Changed("revision") {
		r, err := c.TaxRecord(cmd.Context(), id, false)
		if err != nil {
			return nil, 0, err
		}
		revision = r.Revision
	}
	return c, revision, nil
}
func newTaxRecordAdvanceCommand() *cobra.Command {
	var f eventFlags
	cmd := &cobra.Command{Use: "advance <record-id>", Short: "Advance a tax record", Example: "docbank tax record advance 11111111-1111-4111-8111-111111111111 --state filed --occurred-on 2026-01-01", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, rev, err := eventConnection(cmd, args[0], f.revision)
		if err != nil {
			return err
		}
		in := api.AdvanceTaxRecordRequest{State: f.kind, OccurredOn: f.date, Currency: f.currency, Reference: f.reference, Note: f.note}
		if cmd.Flags().Changed("amount") {
			in.Amount = &f.amount
		}
		m, err := c.AdvanceTaxRecord(cmd.Context(), args[0], rev, in)
		if err != nil {
			return err
		}
		return printTaxEventMutation(cmd, m)
	}}
	f.bind(cmd, "state", "new state")
	cmd.Flags().StringVar(&f.amount, "amount", "", "exact assessment amount")
	cmd.Flags().StringVar(&f.currency, "currency", "", "currency code")
	return cmd
}
func newTaxEventCommand() *cobra.Command {
	group := &cobra.Command{Use: "event", Short: "Manage tax record events", Long: "Append notes, payments or refunds, or void events.\nWrites use a revision fence and are not retried.", Args: taxGroupArgs, Annotations: map[string]string{helpOnlyAnnotation: helpOnlyMarker}, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var f eventFlags
	add := &cobra.Command{Use: "add <record-id>", Short: "Append a tax event", Example: "docbank tax event add 11111111-1111-4111-8111-111111111111 --kind note --occurred-on 2026-01-01\ndocbank tax event add 11111111-1111-4111-8111-111111111111 --kind payment --occurred-on 2026-01-01 --amount 100.00 --currency USD", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, rev, err := eventConnection(cmd, args[0], f.revision)
		if err != nil {
			return err
		}
		in := api.AddTaxEventRequest{Kind: f.kind, OccurredOn: f.date, Currency: f.currency, Reference: f.reference, Note: f.note}
		if cmd.Flags().Changed("amount") {
			in.Amount = &f.amount
		}
		m, err := c.AddTaxEvent(cmd.Context(), args[0], rev, in)
		if err != nil {
			return err
		}
		return printTaxEventMutation(cmd, m)
	}}
	f.bind(add, "kind", "event kind: note, payment, refund")
	add.Flags().StringVar(&f.amount, "amount", "", "exact cash amount")
	add.Flags().StringVar(&f.currency, "currency", "", "currency code")
	var reason string
	var revision int64
	void := &cobra.Command{Use: "void <record-id> <event-id>", Short: "Void a tax event", Example: "docbank tax event void 11111111-1111-4111-8111-111111111111 33333333-3333-4333-8333-333333333333 --reason correction", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if canonical.ValidateUUIDv4(args[1]) != nil {
			return usageError(errors.New("invalid event ID"))
		}
		c, rev, err := eventConnection(cmd, args[0], revision)
		if err != nil {
			return err
		}
		m, err := c.VoidTaxEvent(cmd.Context(), args[0], args[1], rev, api.VoidTaxEventRequest{Reason: reason})
		if err != nil {
			return err
		}
		return printTaxEventMutation(cmd, m)
	}}
	void.Flags().StringVar(&reason, "reason", "", "reason for voiding")
	void.Flags().Int64Var(&revision, "revision", 0, "expected revision (default: current)")
	_ = void.MarkFlagRequired("reason")
	group.AddCommand(add, void)
	return group
}
