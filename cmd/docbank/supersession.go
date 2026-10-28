package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var supersessionReplaces, supersessionNote string
var supersessionRemove bool
var effectiveFrom, effectiveTo string
var effectiveClear bool
var validityAsOf string
var validityJSON bool

var supersedeCmd = &cobra.Command{
	Use:     "supersede <path-or-id>",
	Short:   "Declare or remove a document replacement",
	GroupID: groupDocuments,
	Example: `  docbank supersede /leases/new.pdf --replaces /leases/old.pdf`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if supersessionReplaces == "" {
			return usageError(errors.New("--replaces is required"))
		}
		if supersessionRemove && cmd.Flags().Changed("note") {
			return usageError(errors.New("--remove cannot be combined with --note"))
		}
		successor, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		predecessor, err := parseNodeSelector(supersessionReplaces)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		resolveSuccessor := successor.resolve
		if supersessionRemove {
			resolveSuccessor = successor.resolveIncludingTrash
		}
		next, err := resolveSuccessor(cmd.Context(), c)
		if err != nil {
			return err
		}
		old, err := predecessor.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		var receipt api.ValidityReceipt
		if supersessionRemove {
			receipt, err = c.DeleteSupersession(cmd.Context(), next.ID, old.ID, old.Revision)
		} else {
			note := supersessionNote
			if !cmd.Flags().Changed("note") {
				view, err := c.NodeValidity(cmd.Context(), old.ID, "")
				if err != nil {
					return err
				}
				for _, link := range view.Successors {
					if link.SuccessorID == next.ID {
						note = link.Note
						break
					}
				}
			}
			receipt, err = c.PutSupersession(cmd.Context(), next.ID, old.ID, old.Revision, note)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Document validity updated: %s (changed: %t)\n", formatNodeSelector(next.ID), receipt.Changed)
		if err != nil {
			return fmt.Errorf("writing document validity update: %w", err)
		}
		return nil
	},
}

var effectiveCmd = &cobra.Command{
	Use:     "effective <path-or-id>",
	Short:   "Set or clear the dates a document is in force",
	GroupID: groupDocuments,
	Example: `  docbank effective /leases/new.pdf --from 2025-03-01 --to 2026-02-28`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if effectiveClear && (cmd.Flags().Changed("from") || cmd.Flags().Changed("to")) {
			return usageError(errors.New("--clear cannot be combined with --from or --to"))
		}
		if !effectiveClear && effectiveFrom == "" && effectiveTo == "" {
			return usageError(errors.New("provide --from or --to, or use --clear"))
		}
		if (cmd.Flags().Changed("from") && effectiveFrom == "") || (cmd.Flags().Changed("to") && effectiveTo == "") {
			return usageError(errors.New("dates must be nonempty; use --clear to erase the window"))
		}
		if err := document.ValidateEffectiveWindow(document.EffectiveWindow{From: effectiveFrom, To: effectiveTo}); err != nil {
			return usageError(err)
		}
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		var receipt api.ValidityReceipt
		if effectiveClear {
			receipt, err = c.DeleteEffectiveWindow(cmd.Context(), node.ID, node.Revision)
		} else {
			var view api.NodeValidity
			view, err = c.NodeValidity(cmd.Context(), node.ID, "")
			if err != nil {
				return err
			}
			window := document.EffectiveWindow{From: effectiveFrom, To: effectiveTo}
			if !cmd.Flags().Changed("from") {
				window.From = view.EffectiveFrom
			}
			if !cmd.Flags().Changed("to") {
				window.To = view.EffectiveTo
			}
			receipt, err = c.PutEffectiveWindow(cmd.Context(), node.ID, node.Revision, api.EffectiveWindowRequest{EffectiveFrom: window.From, EffectiveTo: window.To})
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Effective window updated: %s (changed: %t)\n", formatNodeSelector(receipt.Node.ID), receipt.Changed)
		if err != nil {
			return fmt.Errorf("writing effective window update: %w", err)
		}
		return nil
	},
}

var validityCmd = &cobra.Command{
	Use:     "validity <path-or-id>",
	Short:   "Show whether a document is in force on a date",
	GroupID: groupDocuments,
	Example: `  docbank validity /leases/old.pdf --as-of 2024-06-01 --json`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := document.ValidateDate(validityAsOf); err != nil {
			return usageError(err)
		}
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolve(cmd.Context(), c)
		if err != nil {
			return err
		}
		view, err := c.NodeValidity(cmd.Context(), node.ID, validityAsOf)
		if err != nil {
			return err
		}
		if validityJSON {
			return json.MarshalWrite(cmd.OutOrStdout(), view)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (as of %s)\n", view.Node.Path, view.State, view.AsOf)
		if err != nil {
			return fmt.Errorf("writing document validity: %w", err)
		}
		if view.EffectiveFrom != "" || view.EffectiveTo != "" {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Effective window: %s through %s (inclusive; blank ends are open)\n", view.EffectiveFrom, view.EffectiveTo); err != nil {
				return fmt.Errorf("writing document validity: %w", err)
			}
		}
		if view.Undated {
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), "A successor has no effective start date."); err != nil {
				return fmt.Errorf("writing document validity: %w", err)
			}
		}
		for _, link := range view.Predecessors {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Replaces: %s\n", formatNodeSelector(link.PredecessorID)); err != nil {
				return fmt.Errorf("writing document validity: %w", err)
			}
		}
		for _, link := range view.Successors {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Successor: %s\n", formatNodeSelector(link.SuccessorID)); err != nil {
				return fmt.Errorf("writing document validity: %w", err)
			}
		}
		return nil
	},
}

func init() {
	supersedeCmd.Flags().StringVar(&supersessionReplaces, "replaces", "", "predecessor absolute virtual path or id:<positive-decimal>")
	supersedeCmd.Flags().StringVar(&supersessionNote, "note", "", "explanation of the replacement (up to 1024 UTF-8 bytes)")
	supersedeCmd.Flags().BoolVar(&supersessionRemove, "remove", false, "remove the replacement declaration")
	effectiveCmd.Flags().StringVar(&effectiveFrom, "from", "", "inclusive first effective date (YYYY-MM-DD)")
	effectiveCmd.Flags().StringVar(&effectiveTo, "to", "", "inclusive last effective date (YYYY-MM-DD)")
	effectiveCmd.Flags().BoolVar(&effectiveClear, "clear", false, "clear the declared effective window")
	validityCmd.Flags().StringVar(&validityAsOf, "as-of", "", "evaluation date (YYYY-MM-DD; default daemon local date)")
	validityCmd.Flags().BoolVar(&validityJSON, "json", false, "print JSON to stdout")
	rootCmd.AddCommand(supersedeCmd, effectiveCmd, validityCmd)
}
