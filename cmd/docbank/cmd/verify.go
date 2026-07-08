package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Re-hash every stored blob and report corruption",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		blobs, err := v.store.AllBlobs(cmd.Context())
		if err != nil {
			return err
		}
		var ok, bad int
		for _, b := range blobs {
			if err := cmd.Context().Err(); err != nil {
				return fmt.Errorf("verify interrupted: %w", err)
			}
			switch problem := checkBlob(v, b.Hash); problem {
			case "":
				ok++
			default:
				bad++
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", problem, b.Hash)
			}
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d blob(s) ok, %d problem(s)\n", ok, bad)
		if bad > 0 {
			return fmt.Errorf("verify found %d problem(s)", bad)
		}
		return nil
	},
}

// checkBlob returns "", "missing", "corrupt", or "unreadable".
func checkBlob(v *vault, hash string) string {
	f, err := v.blobs.Open(hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unreadable"
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return "corrupt"
	}
	return ""
}

func init() { rootCmd.AddCommand(verifyCmd) }
