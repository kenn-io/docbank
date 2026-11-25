---
last_edited: 2026-11-24
title: Hosted vault providers
description: Connect Docbank to a service that hosts vaults for you, and see which vaults you can open.
---

# Hosted vault providers

A provider is a service that hosts Docbank vaults for you, such as a vault
service run by your organization. Docbank asks the provider's helper, a
program on your computer that the service supplies, which vaults you can
open. The helper speaks for your account on that service, so Docbank needs no
password or token of its own.

Usually the service's own setup command configures the connection for you.
This page explains what that setup saves and how to check it.

## See your connections and vaults

Not in the latest release yet:

```sh
docbank provider list
docbank provider vaults work
```

`provider list` shows each connection's name and address. `provider vaults`
asks that provider's helper for the vaults you can open and prints each vault's
ID and name. Add `--json` to either command for scripts. The JSON from
`provider vaults` has these fields:

- `enabled`: whether the service offers vaults to you at all.
- `can_create`: whether you can create a vault there.
- `vaults`: each vault's `id` on the service, `name`, and Docbank `vault_uuid`.

If the helper reports that you are signed out, sign in to the service's own
application and run the command again.

## Add or replace a connection

The `provider configure` command reads one JSON document from stdin. The
service's setup command normally sends it.

```json
{
  "version": 1,
  "name": "work",
  "origin": "https://vaults.example.org",
  "mount_prefix": "/documents",
  "browser_origin": "https://vaults.example.org",
  "helper": ["/usr/local/bin/example-helper", "--connection", "work"]
}
```

- `name` is how you refer to the connection: 1 to 63 lowercase letters,
  digits, hyphens, or underscores.
- `origin` and `browser_origin` are HTTPS origins: a scheme and host, with no
  path. `mount_prefix` is the absolute path where the service serves vaults.
- `helper` is the helper's absolute path followed by its arguments. Docbank
  runs it directly, without a shell.

Docbank asks the helper for your vaults before it saves anything. When the
helper refuses or fails, the saved connections stay as they were. If another
setup replaces this installation's ID while the helper answers, Docbank saves
nothing and asks you to run the setup again. Configuring a name again replaces
that connection and keeps the others.

## Where connections are saved

Docbank saves connections in `providers.toml` in a `docbank` folder in your
user configuration directory, outside every vault. Set `DOCBANK_CONFIG_HOME`
to an absolute path to use a different folder. The file also holds this
installation's ID, which Docbank sends to every provider so the service can
tell your computers apart. Reconfiguring keeps the ID.

## Write a helper

A helper reads one JSON request on stdin and writes one JSON result on stdout.
Helpers written in Go can use the `go.kenn.io/docbank/docbank/provider`
package: `DecodeRequest` reads and checks the request, and `WriteResponse`
checks and writes the result.

A discovery request names this installation and the connection:

```json
{
  "version": 1,
  "operation": "discover",
  "request_id": "44444444-4444-4444-8444-444444444444",
  "installation_id": "11111111-1111-4111-8111-111111111111",
  "origin": "https://vaults.example.org",
  "mount_prefix": "/documents"
}
```

A successful result echoes the version, operation, and request ID:

```json
{
  "version": 1,
  "operation": "discover",
  "request_id": "44444444-4444-4444-8444-444444444444",
  "status": "ready",
  "discovery": {
    "enabled": true,
    "can_create": true,
    "vaults": [
      {
        "id": "22222222-2222-4222-8222-222222222222",
        "name": "Studio documents",
        "vault_uuid": "33333333-3333-4333-8333-333333333333"
      }
    ]
  }
}
```

Other results carry `status` with an optional `reason` code made of lowercase
letters and underscores, and no `discovery`:

| `status` | Meaning |
| --- | --- |
| `sign_in_required` | The person must sign in to the service first. |
| `denied` | The service refuses this request. |
| `unavailable` | The helper or service cannot answer now, for example `server_unavailable`. |

Rules for the exchange:

- Exit 0 for any result, including a refusal. Exit 2 when the request is
  invalid, before contacting the service. Exit 1 for any other failure.
  Docbank discards stdout when the helper exits nonzero.
- Requests are at most 16 KiB and results at most 256 KiB. Docbank stops a
  helper after 60 seconds and discards its stderr.
- Docbank rejects a result with unknown or repeated fields, a different
  request ID, a vault without both identities, a repeated vault ID, or a vault
  name with control characters.
