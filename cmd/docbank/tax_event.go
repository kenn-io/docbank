package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func printTaxRecordDetail(cmd *cobra.Command, d api.TaxRecordDetail) error {
	if v, _ := cmd.Flags().GetBool("json"); v {
		return writeCLIJSON(cmd.OutOrStdout(), d)
	}
	if err := printTaxRecord(cmd, d.TaxRecord); err != nil {
		return err
	}
	for _, e := range d.Events {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "event %s\tkind: %s\toccurred: %s\tstate: %s\tamount minor: %v\tcurrency: %s\treference: %q\tnote: %q\tvoided: %v\treason: %q\trevision %d\n", e.EventID, e.Kind, e.OccurredOn, e.SetsState, taxValue(e.AmountMinor), e.Currency, e.Reference, e.Note, taxValue(e.VoidedAt), e.VoidReason, e.RecordRevision); err != nil {
			return err
		}
	}
	return nil
}
func printTaxEventMutation(cmd *cobra.Command, m api.TaxEventMutation) error {
	var err error
	if v, _ := cmd.Flags().GetBool("json"); v {
		err = writeCLIJSON(cmd.OutOrStdout(), m)
	} else {
		err = printTaxRecordDetail(cmd, api.TaxRecordDetail{TaxRecord: m.Record, Events: []api.TaxEvent{m.Event}})
	}
	if err != nil {
		return errors.New("tax record change was committed, but output failed; inspect the record before trying again")
	}
	return nil
}

type eventFlags struct {
	kind, date, amount, currency, reference, note, reason string
	revision                                              int64
}

func (f *eventFlags) bind(cmd *cobra.Command, advance bool) {
	if advance {
		cmd.Flags().StringVar(&f.kind, "state", "", "new state")
	} else {
		cmd.Flags().StringVar(&f.kind, "kind", "", "event kind: note")
	}
	cmd.Flags().StringVar(&f.date, "occurred-on", "", "event date")
	cmd.Flags().StringVar(&f.reference, "reference", "", "event reference")
	cmd.Flags().StringVar(&f.note, "note", "", "event note")
	if advance {
		cmd.Flags().StringVar(&f.amount, "amount", "", "exact assessment amount")
		cmd.Flags().StringVar(&f.currency, "currency", "", "currency code")
	}
	cmd.Flags().Int64Var(&f.revision, "revision", 0, "expected revision (default: current)")
	if advance {
		_ = cmd.MarkFlagRequired("state")
	} else {
		_ = cmd.MarkFlagRequired("kind")
	}
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
	f.bind(cmd, true)
	return cmd
}
func newTaxEventCommand() *cobra.Command {
	group := &cobra.Command{Use: "event", Short: "Manage tax record events", Long: "Append notes or void events. Writes use a revision fence and are not retried.", Args: taxGroupArgs, Annotations: map[string]string{helpOnlyAnnotation: helpOnlyMarker}, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var f eventFlags
	add := &cobra.Command{Use: "add <record-id>", Short: "Append a note", Example: "docbank tax event add 11111111-1111-4111-8111-111111111111 --kind note --occurred-on 2026-01-01", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, rev, err := eventConnection(cmd, args[0], f.revision)
		if err != nil {
			return err
		}
		m, err := c.AddTaxEvent(cmd.Context(), args[0], rev, api.AddTaxEventRequest{Kind: f.kind, OccurredOn: f.date, Reference: f.reference, Note: f.note})
		if err != nil {
			return err
		}
		return printTaxEventMutation(cmd, m)
	}}
	f.bind(add, false)
	var reason string
	var revision int64
	void := &cobra.Command{Use: "void <record-id> <event-id>", Short: "Void a tax event", Example: "docbank tax event void 11111111-1111-4111-8111-111111111111 33333333-3333-4333-8333-333333333333 --reason correction", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if err := taxRecordID(args[1]); err != nil {
			return err
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
