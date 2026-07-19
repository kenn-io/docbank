package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const maxTagLimit = 1000

var (
	tagListLimit    int
	tagListOffset   int
	tagListJSON     bool
	tagShowJSON     bool
	tagCreateJSON   bool
	tagRenameJSON   bool
	tagDeleteJSON   bool
	tagAssignJSON   bool
	tagUnassignJSON bool
	tagNodesLimit   int
	tagNodesOffset  int
	tagNodesJSON    bool
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Define tags and organize nodes with them",
}

var tagListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tag definitions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateTagPagination(tagListLimit, tagListOffset); err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.Tags(cmd.Context(), tagListLimit, tagListOffset)
		if err != nil {
			return err
		}
		if tagListJSON {
			return writeTagJSON(cmd.OutOrStdout(), page)
		}
		if len(page.Items) == 0 {
			if page.Total == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no tags")
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"no tags at offset %d (%d total)\n", page.Offset, page.Total)
			}
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tNAME\tREVISION\tASSIGNMENTS")
		for _, tag := range page.Items {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\n",
				tag.ID, tag.Name, tag.Revision, tag.AssignmentCount)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing tags: %w", err)
		}
		writeTagContinuation(cmd.OutOrStdout(), tagListOffset, len(page.Items), page.Total)
		return nil
	},
}

var tagShowCmd = &cobra.Command{
	Use:   "show <name-or-id>",
	Short: "Inspect one tag",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		tag, err := resolveTag(cmd, c, args[0])
		if err != nil {
			return err
		}
		if tagShowJSON {
			return writeTagJSON(cmd.OutOrStdout(), tag)
		}
		return writeTag(cmd.OutOrStdout(), tag)
	},
}

var tagCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Define a tag with a new stable ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		tag, err := c.CreateTag(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if tagCreateJSON {
			return writeTagJSON(cmd.OutOrStdout(), tag)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "created tag %q (%s)\n", tag.Name, tag.ID)
		return nil
	},
}

var tagRenameCmd = &cobra.Command{
	Use:   "rename <name-or-id> <new-name>",
	Short: "Rename a tag without changing its stable ID",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		current, err := resolveTag(cmd, c, args[0])
		if err != nil {
			return err
		}
		tag, err := c.RenameTag(cmd.Context(), current.ID, current.Revision, args[1])
		if err != nil {
			return err
		}
		if tagRenameJSON {
			return writeTagJSON(cmd.OutOrStdout(), tag)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "renamed tag %q to %q (%s)\n",
			current.Name, tag.Name, tag.ID)
		return nil
	},
}

var tagDeleteCmd = &cobra.Command{
	Use:   "delete <name-or-id>",
	Short: "Delete a tag and all of its assignments",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		tag, err := resolveTag(cmd, c, args[0])
		if err != nil {
			return err
		}
		receipt, err := c.DeleteTag(cmd.Context(), tag.ID, tag.Revision)
		if err != nil {
			return err
		}
		if tagDeleteJSON {
			return writeTagJSON(cmd.OutOrStdout(), receipt)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted tag %q (%s); removed %d assignments\n",
			receipt.Tag.Name, receipt.Tag.ID, receipt.RemovedAssignments)
		return nil
	},
}

var tagAssignCmd = &cobra.Command{
	Use:   "assign <name-or-id> <path>",
	Short: "Assign a tag to a live node",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return changeTagAssignmentCLI(cmd, args[0], args[1], true, tagAssignJSON)
	},
}

var tagUnassignCmd = &cobra.Command{
	Use:   "unassign <name-or-id> <path>",
	Short: "Remove a tag from a live node",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return changeTagAssignmentCLI(cmd, args[0], args[1], false, tagUnassignJSON)
	},
}

var tagNodesCmd = &cobra.Command{
	Use:   "nodes <name-or-id>",
	Short: "List live and trashed nodes carrying a tag",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateTagPagination(tagNodesLimit, tagNodesOffset); err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		tag, err := resolveTag(cmd, c, args[0])
		if err != nil {
			return err
		}
		page, err := c.TaggedNodes(cmd.Context(), tag.ID, tagNodesLimit, tagNodesOffset)
		if err != nil {
			return err
		}
		if tagNodesJSON {
			return writeTagJSON(cmd.OutOrStdout(), page)
		}
		if len(page.Items) == 0 {
			if page.Total == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no nodes carry tag %q\n", tag.Name)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"no nodes for tag %q at offset %d (%d total)\n",
					tag.Name, page.Offset, page.Total)
			}
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NODE\tREVISION\tSTATE\tPATH")
		for _, item := range page.Items {
			state, path := "live", item.Path
			if item.Node.TrashedAt != "" {
				state, path = "trashed", "-"
			}
			_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\n",
				item.Node.ID, item.Node.Revision, state, path)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("writing tagged nodes: %w", err)
		}
		writeTagContinuation(cmd.OutOrStdout(), tagNodesOffset, len(page.Items), page.Total)
		return nil
	},
}

func changeTagAssignmentCLI(
	cmd *cobra.Command, selector, path string, assign, jsonOutput bool,
) error {
	if !strings.HasPrefix(path, "/") {
		return usageError(errors.New("tag assignment path must be absolute"))
	}
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	tag, err := resolveTag(cmd, c, selector)
	if err != nil {
		return err
	}
	var receipt api.TagAssignmentReceipt
	if assign {
		receipt, err = c.AssignTagPath(cmd.Context(), tag.ID, path)
	} else {
		receipt, err = c.UnassignTagPath(cmd.Context(), tag.ID, path)
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeTagJSON(cmd.OutOrStdout(), receipt)
	}
	verb := "assigned"
	preposition := "to"
	if !assign {
		verb = "unassigned"
		preposition = "from"
	}
	if !receipt.Changed {
		verb = "already assigned"
		if !assign {
			verb = "already unassigned"
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s tag %q %s %s (revision %d)\n",
		verb, receipt.Tag.Name, preposition, receipt.Node.Path, receipt.Node.Revision)
	return nil
}

func resolveTag(cmd *cobra.Command, c *client.Client, selector string) (api.Tag, error) {
	if client.IsCanonicalUUIDv4(selector) {
		tag, err := c.Tag(cmd.Context(), selector)
		if err != nil {
			return api.Tag{}, fmt.Errorf("resolving tag %q: %w", selector, err)
		}
		return tag, nil
	}
	tag, err := c.TagByName(cmd.Context(), selector)
	if err != nil {
		return api.Tag{}, fmt.Errorf("resolving tag %q: %w", selector, err)
	}
	return tag, nil
}

func validateTagPagination(limit, offset int) error {
	if limit < 1 || limit > maxTagLimit {
		return usageError(fmt.Errorf("--limit must be between 1 and %d", maxTagLimit))
	}
	if offset < 0 {
		return usageError(errors.New("--offset must not be negative"))
	}
	return nil
}

func writeTag(w io.Writer, tag api.Tag) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "ID:\t%s\n", tag.ID)
	_, _ = fmt.Fprintf(tw, "Name:\t%s\n", tag.Name)
	_, _ = fmt.Fprintf(tw, "Revision:\t%d\n", tag.Revision)
	_, _ = fmt.Fprintf(tw, "Assignments:\t%d\n", tag.AssignmentCount)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing tag: %w", err)
	}
	return nil
}

func writeTagJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("writing tag JSON: %w", err)
	}
	return nil
}

func writeTagContinuation(w io.Writer, offset, count, total int) {
	if offset+count < total {
		_, _ = fmt.Fprintf(w, "showing %d-%d of %d (use --offset to continue)\n",
			offset+1, offset+count, total)
	}
}

func init() {
	tagListCmd.Flags().IntVar(&tagListLimit, "limit", 100, "maximum tags to return (1-1000)")
	tagListCmd.Flags().IntVar(&tagListOffset, "offset", 0, "number of tags to skip")
	tagListCmd.Flags().BoolVar(&tagListJSON, "json", false, "emit machine-readable JSON")
	tagShowCmd.Flags().BoolVar(&tagShowJSON, "json", false, "emit machine-readable JSON")
	tagCreateCmd.Flags().BoolVar(&tagCreateJSON, "json", false, "emit machine-readable JSON")
	tagRenameCmd.Flags().BoolVar(&tagRenameJSON, "json", false, "emit machine-readable JSON")
	tagDeleteCmd.Flags().BoolVar(&tagDeleteJSON, "json", false, "emit machine-readable JSON")
	tagAssignCmd.Flags().BoolVar(&tagAssignJSON, "json", false, "emit machine-readable JSON")
	tagUnassignCmd.Flags().BoolVar(&tagUnassignJSON, "json", false, "emit machine-readable JSON")
	tagNodesCmd.Flags().IntVar(&tagNodesLimit, "limit", 100, "maximum nodes to return (1-1000)")
	tagNodesCmd.Flags().IntVar(&tagNodesOffset, "offset", 0, "number of tagged nodes to skip")
	tagNodesCmd.Flags().BoolVar(&tagNodesJSON, "json", false, "emit machine-readable JSON")
	tagCmd.AddCommand(tagListCmd, tagShowCmd, tagCreateCmd, tagRenameCmd,
		tagDeleteCmd, tagAssignCmd, tagUnassignCmd, tagNodesCmd)
	rootCmd.AddCommand(tagCmd)
}
