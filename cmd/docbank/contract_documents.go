package main

import (
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newContractDocumentCommand(link bool) *cobra.Command {
	name := "link"
	if !link {
		name = "unlink"
	}
	var role, version string
	var revision int64
	var jsonOutput bool
	cmd := &cobra.Command{Use: name + " <contract-id> <node>", Short: name + " an exact contract document", Example: "  docbank contract " + name + " <contract-id> id:42 --role signed", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("version") && version == "" {
			return errors.New("content version ID must not be empty")
		}
		if cmd.Flags().Changed("revision") && revision < 1 {
			return errors.New("revision must be positive")
		}
		selector, err := parseNodeSelector(args[1])
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		nodeID := selector.id
		if nodeID == 0 {
			var n api.Node
			if link {
				n, err = selector.resolve(cmd.Context(), c)
			} else {
				n, err = selector.resolveIncludingTrash(cmd.Context(), c)
			}
			if err != nil {
				return err
			}
			nodeID = n.ID
		}
		if revision == 0 {
			v, e := c.Contract(cmd.Context(), args[0], "")
			if e != nil {
				return e
			}
			revision = v.Revision
		}
		var out api.Contract
		if link {
			out, err = c.LinkContractDocument(cmd.Context(), args[0], revision, nodeID, role, version)
		} else {
			out, err = c.UnlinkContractDocument(cmd.Context(), args[0], revision, nodeID, role, version)
		}
		if err != nil {
			return contractMutationError(name+" contract document", err)
		}
		return printContractShow(cmd, out, jsonOutput)
	}}
	cmd.Flags().StringVar(&role, "role", "", "document role: draft, signed, audit_trail, amendment, or attachment")
	_ = cmd.MarkFlagRequired("role")
	cmd.Flags().StringVar(&version, "version", "", "content version ID")
	cmd.Flags().Int64Var(&revision, "revision", 0, "expected revision")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}
func init() {
	contractCmd.AddCommand(newContractDocumentCommand(true), newContractDocumentCommand(false))
}
