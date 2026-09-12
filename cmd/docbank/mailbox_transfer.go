package main

import (
	"encoding/json/v2"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
	"io"
	"os"
)

func newMailboxArchiveCommand() *cobra.Command {
	return &cobra.Command{Use: "register <archive-id> <description>", Short: "Register an application-independent EML archive identity", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return c.RegisterMailboxArchive(cmd.Context(), store.MailboxArchive{ID: args[0], Description: args[1]})
	}}
}

func newMailboxTransferCommand() *cobra.Command {
	var archive, reference, dest, settings string
	var revision int64
	command := &cobra.Command{Use: "transfer <message.eml>", Short: "Transfer one explicitly identified EML with a durable retry receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if archive == "" || reference == "" {
			return store.ErrMailboxInvalid
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		dir, err := c.Stat(cmd.Context(), dest)
		if err != nil {
			return err
		}
		source, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()
		info, err := source.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 128<<20 {
			return store.ErrMailboxLimit
		}
		hash, err := hashMailboxSource(cmd.Context(), source, info.Size())
		if err != nil {
			return err
		}
		if _, err = source.Seek(0, io.SeekStart); err != nil {
			return err
		}
		request := store.MailboxTransferRequest{ArchiveID: archive, Reference: reference, DestinationID: dir.ID, Name: "message.eml", Settings: settings, SHA256: hash, Size: info.Size()}
		if cmd.Flags().Changed("if-rev") {
			if revision < 1 {
				return store.ErrMailboxInvalid
			}
			request.ExpectedRevision = &revision
		}
		receipt, err := c.TransferMailboxEML(cmd.Context(), request, source)
		if err != nil {
			return err
		}
		return json.MarshalWrite(cmd.OutOrStdout(), receipt)
	}}
	command.Flags().StringVar(&archive, "archive", "", "Registered source archive identity")
	command.Flags().StringVar(&reference, "reference", "", "Stable occurrence reference within the archive")
	command.Flags().StringVar(&dest, "dest", "/", "Destination virtual directory")
	command.Flags().StringVar(&settings, "settings", "docbank-eml-transfer/v1", "Stable caller settings identity")
	command.Flags().Int64Var(&revision, "if-rev", 0, "Expected target revision when source bytes change")
	return command
}
