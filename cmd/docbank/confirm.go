package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// confirmPrompt asks a yes-or-no question on w and reads the answer from the
// command's stdin. Only "y" or "yes" confirms; closed input declines.
func confirmPrompt(cmd *cobra.Command, w io.Writer, prompt string) (bool, error) {
	if _, err := fmt.Fprintf(w, "%s [y/N] ", prompt); err != nil {
		return false, fmt.Errorf("writing confirmation prompt: %w", err)
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}
