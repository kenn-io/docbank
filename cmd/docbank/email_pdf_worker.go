package main

import (
	"context"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/emailpdf"
	"os"
	"os/signal"
	"time"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{Use: "internal-email-pdf-worker <chromium> <staging> <version>", Hidden: true, Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		return emailpdf.RunWorker(ctx, args[0], args[1], args[2])
	}})
}
