---
last_edited: 2026-11-28
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

## Check that you can open a vault

Not in the latest release yet:

```sh
docbank provider access work "Studio documents"
docbank provider access work "Studio documents" --actions read,upload --json
```

`provider access` asks the provider's helper to open one vault, renews the
credential once, and ends the open. Name the vault by its ID or its exact
name from `provider vaults`. Use it to confirm that the provider lets this
computer use the vault. It opens no documents.

- `--actions` is what to ask for: `read` (the default), `upload`, or
  `read,upload`.
- When the provider needs your approval, the command prints the page where
  you decide and waits. `--wait` sets how long, 5 minutes by default.
  `--wait 0` does not wait: approve on the page, then run the command again.
- On success it prints the vault's address and when your approval ends. The
  JSON has `vault`, `actions`, `mount`, and `approval_expires_at`. The command
  never prints the credential.

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
The helper contract defines four operations. `provider vaults` and
`provider configure` call `discover`; `provider access` calls all four.

| `operation` | Purpose | Extra request fields |
| --- | --- | --- |
| `discover` | List the vaults the signed-in person can open. | None. |
| `open` | Return a short-lived credential for one vault. | `vault_id`, `vault_uuid`, `actions`. |
| `renew` | Replace an open vault's credential before it expires. | `vault_id`, `vault_uuid`, `actions`, `open_request_id`, `generation`. |
| `release` | End an open so its credentials stop working. | `vault_id`, `vault_uuid`, `actions`, `open_request_id`. |

Helpers written in Go can use `go.kenn.io/docbank/docbank/provider`:

- `DecodeRequest` reads and checks the request on stdin.
- `WriteResponse` checks the result against the request and writes it to stdout.
- `ValidateApprovalURL` checks an approval page address against the
  connection's `browser_origin`.
- `NewDiscoverRequest`, `NewOpenRequest`, `NewRenewRequest`,
  `NewReleaseRequest`, `DecodeResponse`, and `Exchange` build requests and run
  or check exchanges in helper tests.

### Try the example helper

The Docbank repository includes an example helper for a pretend hosting
service. It keeps the service's state in one local JSON file and serves no
documents. Use it to try the provider commands or to compare your helper's
answers. Run these from a checkout of the repository:

```sh
go build -o example-helper ./docbank/provider/exampleservice/helper
./example-helper --state "$PWD/service.json" --vault "Studio documents" init
```

Connect Docbank to it. Both paths in `helper` must be absolute:

```sh
docbank provider configure <<EOF
{"version": 1, "name": "example",
 "origin": "https://vaults.example.org", "mount_prefix": "/documents",
 "browser_origin": "https://vaults.example.org",
 "helper": ["$PWD/example-helper", "--state", "$PWD/service.json"]}
EOF
docbank provider access example "Studio documents" --wait 0
```

The last command reports that the provider is waiting for your approval. The
example service has no web page, so decide from the command line, then check
again:

```sh
./example-helper --state "$PWD/service.json" approve
docbank provider access example "Studio documents"
```

`decline` refuses every waiting open instead. The example approves an
installation for one vault and the actions it asked for, for 90 days, and its
credentials last 15 minutes.

### Requests

Every request carries `version` set to `1`, `operation`, a fresh `request_id`
UUID, this computer's `installation_id` UUID, and the connection's `origin` and
`mount_prefix`.

A discovery request carries only those envelope fields:

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

An `open` request adds the vault identities and requested actions:

```json
{
  "version": 1,
  "operation": "open",
  "request_id": "44444444-4444-4444-8444-444444444444",
  "installation_id": "11111111-1111-4111-8111-111111111111",
  "origin": "https://vaults.example.org",
  "mount_prefix": "/documents",
  "vault_id": "22222222-2222-4222-8222-222222222222",
  "vault_uuid": "33333333-3333-4333-8333-333333333333",
  "actions": ["read", "upload"]
}
```

Rules for the vault and open fields:

- `open`, `renew`, and `release` carry `vault_id`, the service's UUID for the
  vault, and `vault_uuid`, the Docbank vault UUID.
- `actions` is a nonempty, sorted list without repeats. Valid values are
  `"read"` and `"upload"`.
- `renew` and `release` carry `open_request_id`, set to the `request_id` from
  the `open` request that started the open.
- `renew` carries `generation`, an integer of at least `1` naming the
  credential generation to replace.
- Omit fields an operation does not use. `discover` omits all five fields,
  `open` omits `open_request_id` and `generation`, and `release` omits
  `generation`.

### Results

Every result echoes `version`, `operation`, and `request_id`, and sets
`status`.

A ready `discover` result returns `discovery`:

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

A ready `open` or `renew` result returns `access`:

```json
{
  "version": 1,
  "operation": "open",
  "request_id": "44444444-4444-4444-8444-444444444444",
  "status": "ready",
  "access": {
    "actions": ["read", "upload"],
    "mount": "https://vaults.example.org/documents/22222222-2222-4222-8222-222222222222",
    "credential": "example-credential",
    "generation": 1,
    "expires_at": "2030-01-01T00:15:00Z",
    "approval_expires_at": "2030-03-01T00:00:00Z"
  }
}
```

Allowed `status` values depend on the operation. Statuses that allow `reason`
take an optional fixed code of at most 64 lowercase ASCII letters and
underscores, never free text.

| `status` | Operations | Payload | Meaning |
| --- | --- | --- | --- |
| `ready` | `discover`, `open`, `renew` | `discovery` for `discover`; `access` with `credential` for `open` and `renew`. No `reason`. | The request succeeded. |
| `approval_required` | `open` | `approval_url`. No `reason`. | The person has not approved this open yet. They approve in their browser on the service's page. Docbank sends the same `open` request again, with the same `request_id`, until the result changes. |
| `conflict` | `open`, `renew` | `access` without `credential`, optional `reason`. | The service already answered this request with a credential. The result repeats the access facts without `credential`. |
| `released` | `release` | Envelope only. No `reason`. | The open has ended and its credentials stop working. |
| `sign_in_required` | All | Optional `reason`. | The person must sign in to the service first. |
| `denied` | All | Optional `reason`. | The service refuses this request. |
| `unavailable` | All | Optional `reason`, for example `server_unavailable`. | The helper or service cannot answer now. |

### Checks and limits

Docbank enforces these rules on the process and its result:

- Exit 0 for any valid result, including a refusal or conflict. Exit 2 when the
  request is invalid, before contacting the service. Exit 1 for any other
  failure. Docbank discards stdout when the helper exits nonzero.
- Requests are at most 16 KiB and results at most 256 KiB. Docbank stops a
  helper after 60 seconds and discards its stderr.
- Results must be a single JSON object with no unknown fields, no repeated
  keys, matching `version`, `operation`, and `request_id`, and only the fields
  allowed for that status.
- In `discovery`, `vaults` must be a JSON array, each vault must carry both
  `id` and `vault_uuid`, vault `id` values must not repeat, and each `name`
  must be nonempty UTF-8 of at most 1024 bytes without leading or trailing
  whitespace, control characters, or bidi control characters.
- In `access`, `mount` must be an HTTPS address strictly under the connection's
  `origin` and `mount_prefix`, with a clean path and no user information,
  backslashes, query, or fragment. Percent-encode only the characters a URL
  path requires, such as a space as `%20`, with uppercase hex digits.
- In `access`, `actions` must be nonempty, sorted, duplicate-free, and limited
  to actions the request asked for.
- In `access`, `generation` counts credentials for an open starting at `1`, and
  a newer generation replaces every older one. A `renew` result must return a
  `generation` greater than the request's `generation`.
- In `access`, `expires_at` and `approval_expires_at` must be UTC timestamps
  with a zero offset, and `expires_at` must not be after `approval_expires_at`.
- In `access`, `credential` is required on `ready` and must be omitted on
  `conflict`. It is at most 4096 bytes of visible ASCII without spaces, from
  `!` through `~`.
- On `approval_required`, `approval_url` must be an HTTPS address of at most
  2048 bytes with no user information or fragment. It may include a query
  string. `ValidateApprovalURL` also requires it to be on the connection's
  `browser_origin`.
