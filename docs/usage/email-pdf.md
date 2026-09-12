---
last_edited: 2026-09-12
title: Email PDFs
description: Render one exact email version and keep a verified PDF alongside its original EML.
---

# Email PDFs

Render one retained EML version as a PDF without changing the original email,
its attachment documents, or body-search results. No mailbox archive is needed.

```sh
docbank versions list /Mail/synthetic.eml
docbank email-pdf 20000000-0000-4000-8000-000000000001 message.pdf
docbank email-pdf 20000000-0000-4000-8000-000000000001 letter.pdf --paper Letter
```

The command requests local rendering through the daemon, waits for the existing
processing job, and verifies the complete PDF before publishing the local file.
An existing destination requires `--overwrite`. Interrupting the command stops
waiting; an already queued daemon job may still finish.

In the web app, select an email, open **Version history**, and choose its exact
version. **Email PDF** lists retained PDFs for the selected paper size and
offers rendering with the configured recipe. Retained downloads work after
restore even when the renderer is not installed. Different recipes remain
separate; requesting the same source and recipe reuses its retained receipt.

For multiple messages, use [verified export bundles](export-bundles.md).
The export drawer can include retained body PDFs, original attachments and
qualified separately retained nested-email PDFs, with explicit partial and
duplicate policies. It keeps each PDF separate and records every occurrence.

## What is included

The PDF includes Subject, From, To, Cc, available Bcc, original Date with its
timezone, Message-ID, the complete selected body, expanded quoted text,
verified raster inline images, and an attachment inventory. Attachment contents
are not embedded or concatenated. Plain text preserves whitespace and wraps
long lines. Tables paginate; long URLs wrap.

Remote, missing, ambiguous, and unsupported inline resources are visible
placeholders. Unsafe active HTML is removed with a warning. Invalid header or
date data remains visible with warnings. Encrypted, unavailable, incomplete,
invalid UTF-8, or oversized bodies fail instead of producing a partial success.

A4 is the default; US Letter is also supported. Both are portrait with 12 mm
margins. The versioned recipe pins the worker, Chromium bundle, font tree,
sanitizer, body selection, page geometry, locale, timezone, and attachment
policy. Metadata normalization makes repeated output reproducible on the same
pinned runtime; byte identity across different browser builds is not promised.

## Configure the renderer

New rendering requires a Linux daemon, systemd transient services, cgroup v2
memory enforcement, and non-interactive authority to run the renderer's
`systemd-run` and `systemctl` commands. This is an operator-managed local
process, not a sandbox for an untrusted daemon operator. macOS and Windows
daemons return an actionable unavailable response for new rendering; retained
PDF downloads remain available.

Install a standalone Chromium bundle and an ordinary-file-only Noto font tree
outside the vault. Docbank does not download a browser, follow system font
configuration, or use an unpinned browser from `PATH`. Chrome for Testing
151.0.7922.34 with Noto Sans and Noto CJK is the qualified reference runtime.
Keep the complete bundle, not just the `chrome` executable.

Compute each tree's pin with the repository's
[`email-pdf-pins.sh`](https://github.com/kenn-io/docbank/blob/main/scripts/email-pdf-pins.sh)
helper, which matches `document/emailpdf.TreeSHA256`:

```sh
bash scripts/email-pdf-pins.sh /opt/docbank-email-pdf/chrome-linux64
bash scripts/email-pdf-pins.sh /opt/docbank-email-pdf/fonts
```

Set the resulting hashes in the daemon vault's `config.toml`:

```toml
[email_pdf]
chromium = "/opt/docbank-email-pdf/chrome-linux64/chrome"
bundle = "/opt/docbank-email-pdf/chrome-linux64"
bundle_sha256 = "<64-character bundle hash>"
version = "151.0.7922.34"
fonts = "/opt/docbank-email-pdf/fonts"
fonts_sha256 = "<64-character font-tree hash>"
```

Restart the daemon after configuration changes. Missing or mismatched pins
fail explicitly. The daemon also fingerprints its own worker executable;
changing that binary creates a different recipe. Configuration and external
runtime files are not part of a vault backup.

Each of two concurrent workers is limited to 512 MiB with no swap, 60 seconds,
1000 pages, and 256 MiB of PDF output. Network access is blocked by the private
network namespace, browser request blocking, and sanitized document policy.
Cancellation and timeout stop the entire transient unit before staging is
removed. On restart, the exclusive vault owner stops specifically marked
abandoned units before clearing their staging.

The output must pass an independent PDF parser and exact byte/hash checks
before publication. Its receipt binds original version, decoded generation,
recipe, selected body, and PDF. Backup, restore, and GC preserve that retained
authority. This is a readable derivative, not a redaction, Bates-numbering,
or legal-authenticity claim.
