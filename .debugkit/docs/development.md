# Developing kit

## Shared Go build cache

`make` targets build with `-trimpath`, so compiled code records
module-relative source paths (`go.kenn.io/kit/pack/pack.go`) instead of the
absolute path of your checkout. Every checkout and git worktree of kit then
shares one set of Go build cache entries instead of compiling and caching its
own copy of every package.

- The Makefile adds `-trimpath` to any `GOFLAGS` you already set, including a
  value saved with `go env -w GOFLAGS=...`.
- To opt out for one command, run it with `GOFLAGS=-trimpath=false`.
- To get the same sharing from plain `go build` and `go test` outside `make`,
  run `go env -w GOFLAGS=-trimpath` once.

## Debugging trimmed builds

A trimmed binary does not record where its source files live on disk, so Delve
cannot find them without help.

- `dlv debug` and `dlv test` inherit `GOFLAGS`. Pass
  `--build-flags=-trimpath=false` so the debug build records real paths. Debug
  builds disable optimizations, so they never shared cache entries anyway.
- `dlv exec` and `dlv attach` on a trimmed binary: start Delve from the
  repository root and run `config substitute-path -guess` (Delve 1.24 or
  later). It maps the module path to your checkout, the standard library to
  `GOROOT`, and dependencies to the module cache.

## Object storage conformance

The `objectstore` S3 and GCS stores have unit tests against in-process fakes.
Two opt-in conformance tests go further. Each writes under its own random
prefix, removes what it wrote, and never creates or deletes a bucket.

**Loopback S3.** `TestS3ObjectConformance` runs against a disposable
S3-compatible service on localhost, as CI does with RustFS. Set
`KIT_S3_TEST_ENDPOINT` to the service URL; `KIT_S3_TEST_BUCKET`,
`KIT_S3_TEST_ACCESS_KEY`, and `KIT_S3_TEST_SECRET_KEY` override the defaults.
It refuses any endpoint that is not loopback.

**Live providers.** Only these tests show that real AWS S3 and Google Cloud
Storage behave as the stores expect; loopback services and fakes cannot.
Setting a variable is the consent to run against that provider:

| Test                     | Variable                              | Credentials                     |
| ------------------------ | ------------------------------------- | ------------------------------- |
| `TestS3LiveConformance`  | `KIT_S3_LIVE_URL=s3://bucket/prefix`  | AWS SDK default chain           |
| `TestGCSLiveConformance` | `KIT_GCS_LIVE_URL=gs://bucket/prefix` | Application Default Credentials |

S3 also needs the bucket's region in `KIT_S3_LIVE_REGION` or `AWS_REGION`. The
identity needs object create, read, delete, and list on the bucket, plus
aborting multipart uploads on S3. Give the bucket a lifecycle rule that removes
objects after a day, so an interrupted run leaves nothing behind.

```sh
KIT_S3_LIVE_URL=s3://example-bucket/kit AWS_REGION=us-east-2 \
  go test -run TestS3LiveConformance ./objectstore/s3
KIT_GCS_LIVE_URL=gs://example-bucket/kit \
  go test -run TestGCSLiveConformance ./objectstore/gcs
```
