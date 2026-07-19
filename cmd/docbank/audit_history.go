package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var (
	auditHistoryNodeID int64
	auditHistoryLimit  int
	auditHistoryCursor string
	auditHistoryJSON   bool
)

var auditHistoryCmd = &cobra.Command{
	Use:   "history [path]",
	Short: "Read one permanently protected node's canonical event timeline",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nodeIDSet := cmd.Flags().Changed("node-id")
		if (len(args) == 1) == nodeIDSet {
			return usageError(errors.New(
				"audit history requires exactly one path or --node-id"))
		}
		if auditHistoryLimit < 1 || auditHistoryLimit > 500 {
			return usageError(errors.New("audit history --limit must be between 1 and 500"))
		}
		path := ""
		if len(args) == 1 {
			path = args[0]
		}
		if nodeIDSet && auditHistoryNodeID < 1 {
			return usageError(errors.New("audit history --node-id must be positive"))
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return usageError(errors.New("audit history path must be absolute"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.AuditHistory(
			cmd.Context(), path, auditHistoryNodeID, auditHistoryLimit, auditHistoryCursor,
		)
		if err != nil {
			return err
		}
		if auditHistoryJSON {
			return writeAuditJSON(cmd.OutOrStdout(), page)
		}
		return writeAuditHistory(cmd.OutOrStdout(), page)
	},
}

func writeAuditHistory(w io.Writer, page api.AuditEventPage) error {
	coordinate := auditDisplayPath(page.Path)
	if page.Node.TrashedAt != "" {
		coordinate = fmt.Sprintf("node %d in trash", page.Node.ID)
	}
	if _, err := fmt.Fprintf(w, "audit history for %s (node %d): %d recorded event(s)\n",
		coordinate, page.Node.ID, page.Total); err != nil {
		return fmt.Errorf("writing audit history: %w", err)
	}
	if len(page.Items) == 0 {
		if _, err := fmt.Fprintln(w,
			"no events on this page; the node is still permanently protected"); err != nil {
			return fmt.Errorf("writing audit history: %w", err)
		}
	}
	for _, event := range page.Items {
		if _, err := fmt.Fprintf(w, "%s  %s  operation %d.%d  revision %d -> %d\n",
			event.RecordedAt, event.Kind, event.OperationSequence, event.Ordinal,
			event.PriorNodeRevision, event.ResultingNodeRevision); err != nil {
			return fmt.Errorf("writing audit history: %w", err)
		}
		if event.OldPath != nil && event.NewPath != nil {
			if _, err := fmt.Fprintf(w, "  path: %s -> %s\n",
				auditDisplayPath(event.OldPath.Path),
				auditDisplayPath(event.NewPath.Path)); err != nil {
				return fmt.Errorf("writing audit history: %w", err)
			}
		}
		if event.PriorCurrentVersionID != nil || event.ResultingCurrentVersionID != nil {
			if _, err := fmt.Fprintf(w, "  version: %s -> %s\n",
				auditOptionalValue(event.PriorCurrentVersionID),
				auditOptionalValue(event.ResultingCurrentVersionID)); err != nil {
				return fmt.Errorf("writing audit history: %w", err)
			}
		}
		if event.AgentLabel != nil {
			if _, err := fmt.Fprintf(w, "  agent: %s\n", auditDisplayPath(*event.AgentLabel)); err != nil {
				return fmt.Errorf("writing audit history: %w", err)
			}
		}
		if event.Attachment != nil {
			if err := writeAuditAttachment(w, *event.Attachment); err != nil {
				return err
			}
		}
	}
	if page.NextCursor != "" {
		if _, err := fmt.Fprintln(w, "next cursor: "+page.NextCursor); err != nil {
			return fmt.Errorf("writing audit history: %w", err)
		}
	}
	return nil
}

func writeAuditAttachment(w io.Writer, change api.AuditAttachmentChange) error {
	var summary string
	switch change.Kind {
	case "tag_definition":
		summary = fmt.Sprintf("tag %s: %s -> %s", change.Identity.TagID,
			auditTagState(change.Before), auditTagState(change.After))
	case "tag_assignment":
		summary = fmt.Sprintf("tag %s on node %d: %s -> %s",
			change.Identity.TagID, change.Identity.NodeID,
			auditPresence(change.Before), auditPresence(change.After))
	case "provenance":
		summary = fmt.Sprintf("provenance %s: %s -> %s", change.Identity.ProvenanceID,
			auditProvenanceState(change.Before), auditProvenanceState(change.After))
	default:
		return fmt.Errorf("writing audit history: unknown attachment kind %q", change.Kind)
	}
	if _, err := fmt.Fprintln(w, "  "+summary); err != nil {
		return fmt.Errorf("writing audit history: %w", err)
	}
	return nil
}

func auditTagState(state *api.AuditAttachmentState) string {
	if state == nil {
		return "(absent)"
	}
	return auditDisplayPath(state.TagName)
}

func auditPresence(state *api.AuditAttachmentState) string {
	if state == nil {
		return "absent"
	}
	return "present"
}

func auditProvenanceState(state *api.AuditAttachmentState) string {
	if state == nil {
		return "(absent)"
	}
	path := "(path absent)"
	if state.OriginalPath != nil {
		path = auditDisplayPath(*state.OriginalPath)
	}
	return fmt.Sprintf("%s via ingest %s", path, state.IngestID)
}

func auditOptionalValue(value *string) string {
	if value == nil {
		return "(none)"
	}
	return *value
}

func init() {
	auditHistoryCmd.Flags().Int64Var(&auditHistoryNodeID, "node-id", 0,
		"stable node ID, including a node in trash (alternative to path)")
	auditHistoryCmd.Flags().IntVar(&auditHistoryLimit, "limit", 50,
		"maximum events to return (1-500)")
	auditHistoryCmd.Flags().StringVar(&auditHistoryCursor, "cursor", "",
		"opaque continuation cursor from the preceding page")
	auditHistoryCmd.Flags().BoolVar(&auditHistoryJSON, "json", false,
		"machine-readable output")
	auditCmd.AddCommand(auditHistoryCmd)
}
