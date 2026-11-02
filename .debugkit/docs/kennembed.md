# Installing the local embedding server

Apps share one verified server and pinned model under `~/.kenn/embeddings`. Confirmation belongs to each app. Completed installations work offline. Downloading missing files asks for their remaining download size. The installer serializes callers and reserves `.install-*` directories for temporary downloads.

## Publishing bundles

`internal/embedassets.BundleTag` pins the server independently of ordinary kit updates. The initial pin is [v0.47.1](https://github.com/kenn-io/kit/releases/tag/v0.47.1). Its bundles must be published before consumers use the installer.

Run `gh workflow run kennembed.yml --repo kenn-io/kit --field tag=v0.47.1` to build and attach bundles to an existing release. The workflow builds that tag's exact source, checks its embedded source revision, and exercises its native executable. It packages Linux, macOS, and Windows for amd64 and arm64 with runtime libraries and distribution notices, then uploads six archives and `SHA256SUMS`. Pull requests build the pinned bundles as workflow artifacts for inspection and backfill.

Publish a new tag's bundles before changing `BundleTag`. Keep its server, model pins, and runtime filenames aligned. No bundle workflow is required for unrelated kit releases.
