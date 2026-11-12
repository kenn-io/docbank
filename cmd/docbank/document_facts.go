package main

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func documentFactsCommand(kind string) *cobra.Command {
	var asJSON, full bool
	cmd := &cobra.Command{Use: kind + " <path-or-id>", Short: "Show a document's " + kind, Example: "  docbank document " + kind + " /notes.txt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		selector, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := selector.resolveIncludingTrash(cmd.Context(), c)
		if err != nil {
			return err
		}
		if node.CurrentVersionID == "" {
			return errors.New("document has no current content version")
		}
		versionID, err := uuid.Parse(node.CurrentVersionID)
		if err != nil {
			return fmt.Errorf("invalid content version ID: %w", err)
		}
		disclosure := "safe"
		if full {
			disclosure = "full"
		}
		if kind == "dates" {
			d := apiclient.GetVersionDatesQueryDisclosure(disclosure)
			result, err := c.API().GetVersionDates(cmd.Context(), &apiclient.GetVersionDatesRequestOptions{PathParams: &apiclient.GetVersionDatesPath{VersionID: versionID}, Query: &apiclient.GetVersionDatesQuery{Disclosure: &d}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeVersionJSON(cmd.OutOrStdout(), result)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "State: %s\n", result.State)
			for _, diagnostic := range result.Diagnostics {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Diagnostic: %s\n", diagnostic.Detail)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			for _, claim := range result.Claims {
				mark := " "
				if claim.Primary {
					mark = "*"
				}
				_, _ = fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\t%s\n", mark, claim.DateKind, claim.DateValue, claim.ZoneText, claim.Precision, documentFactLabel(claim.ClaimBasis))
			}
			return w.Flush()
		}
		d := apiclient.GetVersionPeopleQueryDisclosure(disclosure)
		result, err := c.API().GetVersionPeople(cmd.Context(), &apiclient.GetVersionPeopleRequestOptions{PathParams: &apiclient.GetVersionPeoplePath{VersionID: versionID}, Query: &apiclient.GetVersionPeopleQuery{Disclosure: &d}})
		if err != nil {
			return err
		}
		if asJSON {
			return writeVersionJSON(cmd.OutOrStdout(), result)
		}
		return writeVersionPeopleTable(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&full, "full", false, "include sensitive claims")
	return cmd
}
func writeVersionPeopleTable(out io.Writer, result *apiclient.VersionPeople) error {
	_, _ = fmt.Fprintf(out, "State: %s\n", result.State)
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	for _, edge := range result.Edges {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", edge.Role, edge.DisplayName, documentFactLabel(edge.Basis))
	}
	for _, assertion := range result.Assertions {
		represented := false
		if assertion.Action == "assert" && assertion.AssertionID != "" {
			for _, edge := range result.Edges {
				if edge.EvidenceKind == "operator_assertion" && edge.EvidenceID == assertion.AssertionID && edge.PersonID == assertion.PersonID && edge.Role == assertion.Role {
					represented = true
					break
				}
			}
		}
		if !represented {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", assertion.Role, assertion.DisplayName, assertion.Action)
		}
	}
	for _, unresolved := range result.Unresolved {
		_, _ = fmt.Fprintf(w, "%s\t%s\tunresolved\n", unresolved.Role, unresolved.RawLabel)
	}
	return w.Flush()
}

func documentFactLabel(value string) string {
	switch value {
	case "source_asserted":
		return "From document"
	case "docbank_observed":
		return "Recorded by Docbank"
	case "identifier_match":
		return "Matched by identity"
	case "external_uid":
		return "External identity"
	case "operator_assigned":
		return "Assigned by you"
	default:
		return value
	}
}

func init() {
	documentCmd.AddCommand(documentFactsCommand("dates"))
	documentCmd.AddCommand(documentFactsCommand("people"))
}
