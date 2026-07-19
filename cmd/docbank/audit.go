package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Protect permanent document history and inspect its evidence",
}

var (
	auditEnableNodeID      int64
	auditEnableAgentLabel  string
	auditEnableRun         bool
	auditEnableToken       string
	auditEnableAcknowledge bool
	auditEnableJSON        bool
)

var auditEnableCmd = &cobra.Command{
	Use:   "enable [path]",
	Short: "Preview permanent retention, then explicitly enable the reviewed scope",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if auditEnableRun {
			if len(args) != 0 || auditEnableNodeID != 0 || auditEnableAgentLabel != "" {
				return usageError(errors.New(
					"audit enable --run uses only the reviewed preview token"))
			}
			if auditEnableToken == "" {
				return usageError(errors.New(
					"audit enable --run requires --token from a fresh preview"))
			}
			if !auditEnableAcknowledge {
				return usageError(errors.New(
					"audit enable --run requires --acknowledge-permanent-retention",
				))
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			status, err := c.EnableAudit(cmd.Context(), auditEnableToken, true)
			if err != nil {
				return err
			}
			if auditEnableJSON {
				return writeAuditJSON(cmd.OutOrStdout(), status)
			}
			return writeAuditEnabled(cmd.OutOrStdout(), status)
		}
		if auditEnableToken != "" || auditEnableAcknowledge {
			return usageError(errors.New(
				"--token and --acknowledge-permanent-retention require --run"))
		}
		if (len(args) == 0) == (auditEnableNodeID == 0) {
			return usageError(errors.New(
				"audit enable preview requires exactly one path or --node-id"))
		}
		if auditEnableNodeID < 0 {
			return usageError(errors.New("audit enable --node-id must be positive"))
		}
		if len(args) == 1 && !strings.HasPrefix(args[0], "/") {
			return usageError(errors.New("audit enable path must be absolute"))
		}
		opts := client.AuditPreviewOptions{
			NodeID: auditEnableNodeID, AgentLabel: auditEnableAgentLabel,
		}
		if len(args) == 1 {
			opts.Path = args[0]
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		preview, err := c.PreviewAudit(cmd.Context(), opts)
		if err != nil {
			return err
		}
		if auditEnableJSON {
			return writeAuditJSON(cmd.OutOrStdout(), preview)
		}
		return writeAuditPreview(cmd.OutOrStdout(), preview)
	},
}

var (
	auditStatusNodeID int64
	auditStatusJSON   bool
)

var auditStatusCmd = &cobra.Command{
	Use:   "status [path]",
	Short: "Inspect vault audit authority and optional node protection",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 && auditStatusNodeID != 0 {
			return usageError(errors.New(
				"audit status accepts either a path or --node-id, not both"))
		}
		path := ""
		if len(args) == 1 {
			path = args[0]
		}
		if auditStatusNodeID < 0 {
			return usageError(errors.New("audit status --node-id must be positive"))
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return usageError(errors.New("audit status path must be absolute"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		status, err := c.AuditStatus(cmd.Context(), path, auditStatusNodeID)
		if err != nil {
			return err
		}
		if auditStatusJSON {
			return writeAuditJSON(cmd.OutOrStdout(), status)
		}
		return writeAuditStatus(cmd.OutOrStdout(), status)
	},
}

func writeAuditPreview(w io.Writer, preview api.AuditEnrollmentPreview) error {
	if _, err := fmt.Fprintln(w, "Audit enrollment preview — no changes made"); err != nil {
		return fmt.Errorf("writing audit preview: %w", err)
	}
	lines := []string{
		fmt.Sprintf("Target: %s (node %d)", auditDisplayPath(preview.TargetPath), preview.TargetNodeID),
		"Permanent scope: " + preview.ScopeID,
		fmt.Sprintf("Protected tree: %d node(s) — %d directories, %d file(s)",
			preview.MemberCount, preview.DirectoryCount, preview.FileCount),
		fmt.Sprintf("Retained versions: %d version(s), %d logical byte(s), %d unique blob(s), %d unique byte(s)",
			preview.VersionCount, preview.LogicalVersionBytes,
			preview.UniqueBlobs, preview.UniqueBlobBytes),
		fmt.Sprintf("Vault-wide permanent metadata: %d topology node(s), %d attached metadata record(s)",
			preview.VaultTopologyNodes, preview.VaultAttachmentRecords),
		fmt.Sprintf("Projected audit JSONL growth: %d byte(s)", preview.AuthorityJSONBytes),
		"Baseline digest: " + preview.BaselineDigest,
	}
	if preview.UnresolvedTrashOrigins > 0 {
		lines = append(lines, fmt.Sprintf("Unresolved retained trash origins: %d",
			preview.UnresolvedTrashOrigins))
	}
	lines = append(lines,
		"This commitment permanently retains protected history plus names, topology, tags, assignments, ingests, and provenance across the entire vault, including outside the selected scope.",
		"Ordinary commands cannot disable audit or purge that retained authority.",
		"Preview expires: "+preview.ExpiresAt,
		"To enable exactly this reviewed scope:",
		fmt.Sprintf("  docbank audit enable --run --token %s --acknowledge-permanent-retention",
			preview.PreviewToken),
	)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("writing audit preview: %w", err)
		}
	}
	return nil
}

func writeAuditEnabled(w io.Writer, status api.AuditStatus) error {
	if len(status.Scopes) != 1 {
		return errors.New("enabled audit status does not contain exactly one first scope")
	}
	scope := status.Scopes[0]
	path := auditDisplayPath(scope.TargetPath)
	if scope.TargetTrashed {
		path = "(target is in trash)"
	}
	_, err := fmt.Fprintf(w,
		"enabled permanent audit scope %s for %s (node %d); protecting %d node(s)\n",
		scope.ID, path, scope.TargetNodeID, scope.MemberCount)
	if err != nil {
		return fmt.Errorf("writing audit result: %w", err)
	}
	return nil
}

func writeAuditStatus(w io.Writer, status api.AuditStatus) error {
	if !status.Enabled {
		if _, err := fmt.Fprintln(w, "audit is not enabled for this vault"); err != nil {
			return fmt.Errorf("writing audit status: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(w,
			"audit enabled; lineage %s, operation %d, allocation entry %d\n",
			status.LineageID, status.OperationSequenceHighWater,
			status.AllocationEntryCount); err != nil {
			return fmt.Errorf("writing audit status: %w", err)
		}
		if _, err := fmt.Fprintf(w, "allocation head: %s\n", status.AllocationHead); err != nil {
			return fmt.Errorf("writing audit status: %w", err)
		}
		for _, scope := range status.Scopes {
			path := auditDisplayPath(scope.TargetPath)
			if scope.TargetTrashed {
				path = "(target is in trash)"
			}
			if _, err := fmt.Fprintf(w,
				"scope %s: %s (node %d), %d member(s), chain entry %d\n"+
					"  baseline: %s\n  chain head: %s\n",
				scope.ID, path, scope.TargetNodeID, scope.MemberCount, scope.EntryCount,
				scope.BaselineDigest, scope.ChainHead); err != nil {
				return fmt.Errorf("writing audit status: %w", err)
			}
		}
	}
	if status.Membership != nil {
		member := status.Membership
		coordinate := auditDisplayPath(member.Path)
		if member.Trashed {
			coordinate = fmt.Sprintf("node %d in trash", member.NodeID)
		}
		if member.Protected {
			_, err := fmt.Fprintf(w, "%s is permanently protected by %d scope(s): %v\n",
				coordinate, len(member.ScopeIDs), member.ScopeIDs)
			if err != nil {
				return fmt.Errorf("writing audit membership: %w", err)
			}
		} else if _, err := fmt.Fprintf(w, "%s is not in an audit scope\n", coordinate); err != nil {
			return fmt.Errorf("writing audit membership: %w", err)
		}
	}
	return nil
}

func auditDisplayPath(path string) string {
	return strconv.QuoteToASCII(path)
}

func writeAuditJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("writing audit JSON: %w", err)
	}
	return nil
}

func init() {
	auditEnableCmd.Flags().Int64Var(&auditEnableNodeID, "node-id", 0,
		"stable live directory ID (alternative to path)")
	auditEnableCmd.Flags().StringVar(&auditEnableAgentLabel, "agent-label", "",
		"optional actor label retained in the enrollment event")
	auditEnableCmd.Flags().BoolVar(&auditEnableRun, "run", false,
		"permanently enable the reviewed preview")
	auditEnableCmd.Flags().StringVar(&auditEnableToken, "token", "",
		"one-use token returned by a fresh preview")
	auditEnableCmd.Flags().BoolVar(&auditEnableAcknowledge,
		"acknowledge-permanent-retention", false,
		"confirm permanent protected history and vault-wide metadata, including names, topology, tags, assignments, ingests, and provenance outside the selected scope")
	auditEnableCmd.Flags().BoolVar(&auditEnableJSON, "json", false, "machine-readable output")
	auditStatusCmd.Flags().Int64Var(&auditStatusNodeID, "node-id", 0,
		"inspect one stable node ID (alternative to path)")
	auditStatusCmd.Flags().BoolVar(&auditStatusJSON, "json", false, "machine-readable output")
	auditCmd.AddCommand(auditEnableCmd, auditStatusCmd)
	rootCmd.AddCommand(auditCmd)
}
