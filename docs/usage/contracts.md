# DocuSeal connection

Docbank can check an optional DocuSeal connection and list its templates. This
connection does not send documents or create signing requests. Configure
`[integrations.docuseal]` as described in [Configuration](../configuration.md#docuseal-connection),
then restart the daemon.

```console
docbank contract sign check
docbank contract sign templates
```

Both commands accept `--json`. Template listing returns a continuation ID when
more results are available; pass it with `--after`. Template names and roles
come from the server and should be treated as untrusted text.
