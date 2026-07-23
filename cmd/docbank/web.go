package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	docweb "go.kenn.io/docbank/internal/web"
)

var webNoBrowser bool

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the local web application",
	Long: `Start or reconnect to the current vault's daemon and open its web application.

The browser receives a daemon-lifetime, read-only session in a URL fragment,
which is not sent in the initial HTTP request. The vault's master API key
never enters the browser.

With --no-browser, the authenticated URL is printed instead. It contains the
session key: do not paste it into logs, issue trackers, or chat.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !docweb.Available() {
			return errors.New("this binary does not contain the web application; rebuild with make build")
		}
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		cfg, err := config.Load(layout.Root)
		if err != nil {
			return err
		}
		if !cfg.Web.Enabled {
			return errors.New("the web application is disabled by [web] enabled = false")
		}
		c, err := client.EnsureWeb(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return runWeb(cmd.Context(), cmd.OutOrStdout(), layout.Root, c, webNoBrowser, openWebBrowser)
	},
}

func runWeb(
	ctx context.Context,
	out io.Writer,
	root string,
	c *client.Client,
	noBrowser bool,
	open func(context.Context, string) error,
) error {
	rawURL, err := c.WebSessionURL(ctx)
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
	launchURL, err := docweb.WriteBootstrap(root, rawURL)
	if err != nil {
		return err
	}
	if err := open(ctx, launchURL); err != nil {
		return fmt.Errorf("opening Docbank web application: %w", err)
	}
	base := strings.SplitN(rawURL, "#", 2)[0]
	_, err = fmt.Fprintf(out, "opened Docbank web application at %s\n", base)
	if err != nil {
		return fmt.Errorf("printing web application status: %w", err)
	}
	return nil
}

func validateWebLaunchURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing web URL: %w", err)
	}
	if u.Scheme != "file" || u.Host != "" || u.Path == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("web launch URL must name a local file without credentials")
	}
	return nil
}

func init() {
	webCmd.Flags().BoolVar(&webNoBrowser, "no-browser", false,
		"print the authenticated URL instead of opening it (contains the session key)")
	rootCmd.AddCommand(webCmd)
}
