---
last_edited: 2026-09-20
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

## What is included

The PDF includes Subject, From, To, Cc, available Bcc, original Date with its
timezone, Message-ID, the complete selected body, expanded quoted text,
verified raster inline images, and an attachment inventory. Attachment contents
are not embedded or concatenated. Plain text preserves whitespace and wraps
long lines. Tables paginate; long URLs wrap.

Remote, missing, ambiguous, and unsupported inline resources are visible
placeholders. Images embedded directly in HTML as `data:` URLs are labeled
unsupported; only verified raster MIME parts referenced by `cid:` are included.
Unsafe active HTML is removed with a warning. Invalid header or date data
remains visible with warnings. Encrypted, unavailable, incomplete,
invalid UTF-8, or oversized bodies fail instead of producing a partial success.

A4 is the default; US Letter is also supported. Both are portrait with 12 mm
margins. The versioned recipe pins the worker, Chromium bundle, font tree,
bubblewrap, sanitizer, body selection, page geometry, locale, timezone, and
attachment policy. Metadata normalization makes repeated output reproducible on the same
pinned runtime; byte identity across different browser builds is not promised.

## Configure the renderer

New rendering requires a Linux daemon with a working systemd **user manager**,
which runs temporary services for the daemon's account. That account needs
cgroup v2 memory and process-count (`pids`) delegation so systemd can enforce
the renderer's limits. Docbank uses `systemd-run --user` and `systemctl --user`;
the daemon needs no `sudo` authority.

For a headless system service with `User=docbank`, an administrator can keep
the account's user manager running without an interactive login:

```sh
loginctl enable-linger docbank
```

Give that service `XDG_RUNTIME_DIR=/run/user/<uid>`, replacing `<uid>` with the
account's numeric UID. If the default user bus is not discovered, also set
`DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus`. These values must name
the daemon account's manager.

Install the distribution's bubblewrap package (`bwrap`) with its user-namespace
support and any required distribution AppArmor profile. Docbank finds the
installed `bwrap` on the daemon's `PATH` and fingerprints its executable; no
extra configuration key is needed.

Chromium also needs permission to create its own nested user namespaces inside
bubblewrap. A distribution's bubblewrap allowlist alone may not permit this,
and an AppArmor rule for Chrome alone may still inherit bubblewrap's restrictions.
An administrator must provide policy that permits the pinned renderer's nested
sandbox under the applicable profiles; otherwise rendering stays unavailable.
See Chromium's [AppArmor guidance](https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md)
for background. Keep both sandboxes enabled; do not disable AppArmor or its
user-namespace restrictions globally.

Install a standalone Chromium bundle and an ordinary-file-only Noto font tree
outside the vault. Docbank does not download a browser, follow system font
configuration, or use an unpinned browser from `PATH`. Chrome for Testing
151.0.7922.34 with Noto Sans and Noto CJK is the selected reference runtime.
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

Restart the daemon after configuration changes. After validating the configured
pins, Docbank probes the user manager, resource limits, bubblewrap isolation,
and an actual Chromium render. If a runtime probe fails, the daemon logs the
reason and new rendering requests return HTTP 503 with an explanation.
The daemon and retained PDF downloads remain available, including on macOS and
Windows. Incorrect configured pins remain startup configuration errors.
The daemon also fingerprints its own worker executable.
Replacing the worker or installed `bwrap` creates a different recipe.
Configuration and external runtime files are not part of a vault backup.

Each of two concurrent workers is limited to 512 MiB with no swap, 60 seconds,
1000 pages, and 256 MiB of PDF output. Pin checks run before the rendering time
limit starts. The renderer sees its pinned runtime and fonts, plus read-only
`/usr`, `/lib`, `/lib64`, and `/etc/ld.so.cache` for system libraries. Its writable
files are limited to the staged input/output directory and private temporary
storage. It cannot see the vault, the user's home, or the host network.

Rendering stages files in the vault's separate `email-pdf-spool/` directory.
Cancellation and timeout stop the entire user service before staging is
removed. On restart, Docbank stops specifically marked abandoned services
before clearing their staging. If it cannot confirm that a service stopped,
it logs the error and leaves those files in place for a later recovery attempt.

The output must pass an independent PDF parser and exact byte/hash checks
before publication. Its receipt binds original version, decoded generation,
recipe, selected body, and PDF. Backup, restore, and GC preserve that retained
authority. This is a readable derivative, not a redaction, Bates-numbering,
or legal-authenticity claim.
