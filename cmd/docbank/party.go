package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newPartyCommand() *cobra.Command {
	root := &cobra.Command{Use: "party", Short: "Manage reusable entities and counterparties", GroupID: groupDocuments}
	var jsonOutput bool
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	printParty := func(cmd *cobra.Command, p api.Party) error {
		if jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), p)
		}
		country, person, retiredAt := "", "", ""
		if p.Country != nil {
			country = *p.Country
		}
		if p.PersonID != nil {
			person = *p.PersonID
		}
		if p.RetiredAt != nil {
			retiredAt = *p.RetiredAt
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tlegal name: %s\tcountry: %s\town: %t\tperson: %s\tretired: %s\trevision %d\tcreated: %s\tupdated: %s\n", p.PartyID, p.Kind, p.DisplayName, p.LegalName, country, p.Own, person, retiredAt, p.Revision, p.CreatedAt, p.UpdatedAt)
		return err
	}
	var kind string
	var own bool
	var retired bool
	var limit, offset int
	list := &cobra.Command{Use: "list", Short: "List parties", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var ownFilter *bool
		if cmd.Flags().Changed("own") {
			ownFilter = &own
		}
		page, err := c.Parties(cmd.Context(), kind, ownFilter, retired, limit, offset)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, p := range page.Items {
			if err := printParty(cmd, p); err != nil {
				return err
			}
		}
		return nil
	}}
	list.Flags().StringVar(&kind, "kind", "", "Filter person or organization")
	list.Flags().BoolVar(&own, "own", false, "Filter own entities")
	list.Flags().BoolVar(&retired, "include-retired", false, "Include retired parties")
	list.Flags().IntVar(&limit, "limit", 100, "Maximum number of parties")
	list.Flags().IntVar(&offset, "offset", 0, "Number of parties to skip")
	show := &cobra.Command{Use: "show <party-id>", Short: "Inspect a party", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		p, err := c.Party(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return printParty(cmd, p)
	}}
	var createKind, createLegal, createCountry, createPerson string
	var createOwn bool
	create := &cobra.Command{Use: "create <display-name>", Short: "Create a party", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input := api.CreatePartyRequest{Kind: createKind, DisplayName: args[0], LegalName: createLegal, Own: createOwn}
		if cmd.Flags().Changed("country") {
			input.Country = &createCountry
		}
		if cmd.Flags().Changed("person") {
			input.PersonID = &createPerson
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		p, err := c.CreateParty(cmd.Context(), input)
		if err != nil {
			return err
		}
		return printParty(cmd, p)
	}}
	create.Flags().StringVar(&createKind, "kind", "organization", "Party kind: person or organization")
	create.Flags().StringVar(&createLegal, "legal-name", "", "Legal name")
	create.Flags().StringVar(&createCountry, "country", "", "ISO 3166-1 alpha-2 country")
	create.Flags().StringVar(&createPerson, "person", "", "Linked person ID")
	create.Flags().BoolVar(&createOwn, "own", false, "This party is one of my entities")
	var editKind, editName, editLegal, editCountry, editPerson string
	var editOwn bool
	var clearCountry, clearPerson bool
	var editRevision int64
	edit := &cobra.Command{Use: "edit <party-id>", Short: "Edit a party", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		input := api.PatchPartyRequest{ClearCountry: clearCountry, ClearPerson: clearPerson}
		if f.Changed("kind") {
			input.Kind = &editKind
		}
		if f.Changed("name") {
			input.DisplayName = &editName
		}
		if f.Changed("legal-name") {
			input.LegalName = &editLegal
		}
		if f.Changed("own") {
			input.Own = &editOwn
		}
		if f.Changed("country") {
			input.Country = &editCountry
		}
		if f.Changed("person") {
			input.PersonID = &editPerson
		}
		if clearCountry && input.Country != nil || clearPerson && input.PersonID != nil {
			return fmt.Errorf("cannot set and clear the same field")
		}
		if input.Kind == nil && input.DisplayName == nil && input.LegalName == nil && input.Own == nil && input.Country == nil && input.PersonID == nil && !clearCountry && !clearPerson {
			return fmt.Errorf("provide at least one change")
		}
		revision := editRevision
		if cmd.Flags().Changed("revision") && revision <= 0 {
			return fmt.Errorf("revision must be positive")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("revision") {
			p, err := c.Party(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			revision = p.Revision
		}
		p, err := c.UpdateParty(cmd.Context(), args[0], revision, input)
		if err != nil {
			return err
		}
		return printParty(cmd, p)
	}}
	edit.Flags().StringVar(&editKind, "kind", "", "Party kind")
	edit.Flags().StringVar(&editName, "name", "", "Display name")
	edit.Flags().StringVar(&editLegal, "legal-name", "", "Legal name")
	edit.Flags().StringVar(&editCountry, "country", "", "ISO country")
	edit.Flags().StringVar(&editPerson, "person", "", "Linked person ID")
	edit.Flags().BoolVar(&editOwn, "own", false, "Whether this is one of my entities")
	edit.Flags().BoolVar(&clearCountry, "clear-country", false, "Remove country")
	edit.Flags().BoolVar(&clearPerson, "clear-person", false, "Remove linked person")
	edit.Flags().Int64Var(&editRevision, "revision", 0, "Expected revision (default: current)")
	var retireRevision int64
	retire := &cobra.Command{Use: "retire <party-id>", Short: "Retire a party", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		revision := retireRevision
		if cmd.Flags().Changed("revision") && revision <= 0 {
			return fmt.Errorf("revision must be positive")
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("revision") {
			p, err := c.Party(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			revision = p.Revision
		}
		p, err := c.RetireParty(cmd.Context(), args[0], revision)
		if err != nil {
			return err
		}
		return printParty(cmd, p)
	}}
	retire.Flags().Int64Var(&retireRevision, "revision", 0, "Expected revision (default: current)")
	root.AddCommand(list, show, create, edit, retire)
	return root
}
func init() { rootCmd.AddCommand(newPartyCommand()) }
