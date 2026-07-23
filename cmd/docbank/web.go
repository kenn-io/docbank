package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var webNoBrowser bool

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the local web application",
	Long: `Start or reconnect to the current vault's daemon and open its web application.

The browser receives the daemon API key in a URL fragment, which is not sent
over HTTP. The application removes it from the address bar and retains it only
for that browser tab's session.

With --no-browser, the authenticated URL is printed instead. It contains the
session key: do not paste it into logs, issue trackers, or chat.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return runWeb(cmd.Context(), cmd.OutOrStdout(), c, webNoBrowser, openWebBrowser)
	},
}

func runWeb(
	ctx context.Context,
	out io.Writer,
	c *client.Client,
	noBrowser bool,
	open func(context.Context, string) error,
) error {
	rawURL, err := c.WebURL()
	if err != nil {
		return err
	}
	if noBrowser {
		_, err = fmt.Fprintln(out, rawURL)
		if err != nil {
			return fmt.Errorf("printing authenticated web URL: %w", err)
		}
		return nil
	}
	if err := open(ctx, rawURL); err != nil {
		return fmt.Errorf("opening Docbank web application: %w", err)
	}
	base := strings.SplitN(rawURL, "#", 2)[0]
	_, err = fmt.Fprintf(out, "opened Docbank web application at %s\n", base)
	if err != nil {
		return fmt.Errorf("printing web application status: %w", err)
	}
	return nil
}

func validateWebURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing web URL: %w", err)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "http" || u.Host == "" ||
		(!strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback())) {
		return errors.New("web URL must use HTTP on a loopback address")
	}
	values, err := url.ParseQuery(u.Fragment)
	if err != nil || values.Get("api_key") == "" {
		return errors.New("web URL is missing its session key")
	}
	return nil
}

func init() {
	webCmd.Flags().BoolVar(&webNoBrowser, "no-browser", false,
		"print the authenticated URL instead of opening it (contains the session key)")
	rootCmd.AddCommand(webCmd)
}
