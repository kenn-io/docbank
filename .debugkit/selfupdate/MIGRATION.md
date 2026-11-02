# Migration Notes

`go.kenn.io/kit/selfupdate` is intended to replace the duplicated
`internal/update` packages in `agentsview` and `msgvault`.

## Minimal Integration

Create a `selfupdate.Client` in each CLI update command:

```go
client := selfupdate.Client{
	Owner:                  "wesm",
	Repo:                   "agentsview", // or "msgvault"
	BinaryName:             "agentsview", // or "msgvault"
	CurrentVersion:         version,
	CacheDir:               appCacheDir,
	GitHubToken:            selfupdate.EnvironmentGitHubToken(), // optional API fallback auth
	AllowUnsignedChecksums: true, // current CLI releases publish SHA256SUMS only
}
```

Use `client.Check(ctx, selfupdate.CheckOptions{Force: force})` where the
current command calls `CheckForUpdate`. A nil result still means no update is
available. If `info.NeedsRefetch()` is true and the user chooses to install,
call `Check` again with `Force: true` before installing.

Use `client.Install(ctx, info, selfupdate.InstallOptions{Progress: progress})`
where the current command calls `PerformUpdate`. CLI output, config loading,
confirmation prompts, and command wiring should stay in the application.

## Release Discovery

By default, `Check` avoids unauthenticated `api.github.com` release discovery.
It follows `https://github.com/<owner>/<repo>/releases/latest` to the release
tag, constructs the conventional archive URL, and reads `SHA256SUMS` from the
release downloads. If that web path fails, it falls back to the GitHub REST API.
Set `GitHubToken` to authenticate only the API fallback request; kit never sends
that token to release asset or checksum download URLs.

Set `ReleaseManifestURL` when a project publishes a static latest-release JSON
document, such as from a docs site or CDN. The smallest useful manifest only
needs the current release tag:

```json
{
  "tag_name": "v1.2.3"
}
```

With only a tag, kit uses the same conventional release asset and `SHA256SUMS`
URLs as web redirect discovery. Projects with custom asset URLs can instead
publish the same compact shape as the GitHub release fields kit consumes:

```json
{
  "tag_name": "v1.2.3",
  "assets": [
    {
      "name": "agentsview_1.2.3_darwin_arm64.tar.gz",
      "size": 123456,
      "browser_download_url": "https://github.com/kenn-io/agentsview/releases/download/v1.2.3/agentsview_1.2.3_darwin_arm64.tar.gz"
    },
    {
      "name": "SHA256SUMS",
      "browser_download_url": "https://github.com/kenn-io/agentsview/releases/download/v1.2.3/SHA256SUMS"
    }
  ]
}
```

When `ReleaseManifestURL` is set, kit uses it directly instead of probing
GitHub's web or API endpoints. `GitHubWebBaseURL` and `GitHubAPIBaseURL` remain
available for tests and GitHub Enterprise installs.

Install verification fails closed by default and requires signed update
metadata. The current agentsview and msgvault CLI release workflows publish
archives plus `SHA256SUMS`, but not CLI update signatures or embedded public
keys, so their initial migrations should set `AllowUnsignedChecksums: true` to
preserve the existing checksum-based integrity check until release signing is
added.

Repos that already publish signed update metadata, or add it later, should
configure `TrustedPublicKeys` or `RequireSignature` and leave
`AllowUnsignedChecksums` false. Signed releases should publish a detached
Ed25519 signature asset named `<asset>.sha256.sig` or `<asset>.sig`; it must
sign the canonical `selfupdate.SignaturePayload` metadata for the archive,
including owner, repo, version, asset name, target platform, and lowercase
SHA-256 checksum.

## App-Specific Details

`agentsview` can pass its existing cache directory directly. `msgvault` should
continue using its app home directory as `CacheDir`; the kit package writes
`update_check.json` with `0600` permissions.

The default asset naming convention matches both source packages:
`<binary>_<version>_<goos>_<goarch>.tar.gz`, with `.zip` on Windows. Repos with
different release asset names can provide `Client.AssetName`.
