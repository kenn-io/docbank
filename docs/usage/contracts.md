# DocuSeal connection

Docbank can check an optional DocuSeal connection and list its templates. This
connection does not send documents or create signing requests. Configure
`[integrations.docuseal]` as described in [Configuration](../configuration.md#docuseal-connection),
then restart the daemon.

```console
docbank contract sign check
docbank contract sign templates
```

Both commands accept `--json`. A full page returns a continuation ID; pass it with `--after` to check for
another page. A full final page can be followed by an empty page. Template names
are display text, capped at 512 bytes with control characters replaced. Roles
are exact identities for later signing: invalid or oversized roles cause the
page to fail rather than being rewritten. The response is limited to 2 MiB,
100 templates per page, 1,024 roles per template and 4,096 bytes per role.
`template_submission` names the configured integration mode; it is not a
probe for optional PDF submission support.
