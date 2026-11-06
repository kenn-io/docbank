---
title: Run a bounded conversion worker
description: Keep conversion off the vault host with a private durable spool, bounded Docling jobs, and repeatable result retrieval.
---

# Run a bounded conversion worker

A conversion worker keeps document conversion and speech transcription off the
vault host. Originals stay in Docbank. The worker retains temporary inputs and
results in its own private directory and sends conversion requests to a
separately installed Docling Serve on the same machine.

This is an opt-in operator service. Building it does not install, start, or
configure a service, change authentication, or keep a computer awake. An Apple
Silicon Mac can run it; having `uv` and `ffmpeg` installed does not qualify a
Docling model or its output.

## Prepare the backend

Use an exclusive Docling Serve deployment bound to a numeric loopback address.
Set `UVICORN_WORKERS=1`, `DOCLING_SERVE_ENG_LOC_NUM_WORKERS=1`, and
`DOCLING_NUM_THREADS=4`. The gateway serializes backend requests. The backend's
single conversion worker also prevents an accepted request whose response was
lost from running alongside its replacement.

Keep remote services and external plugins disabled. Set
`DOCLING_SERVE_ENABLE_REMOTE_SERVICES=false` and
`DOCLING_SERVE_ALLOW_EXTERNAL_PLUGINS=false`. Qualify the exact document and
ASR versions before processing documents. Use separate API keys for Docbank's
worker connection and the loopback backend.

The backend must answer `GET /ready` with HTTP 200 and
`Content-Type: application/json` when ready. The
[qualified Docling Serve 1.32.0 route](https://github.com/docling-project/docling-serve/blob/v1.32.0/docling_serve/app.py#L732)
returns 503 while its models, queue processor or orchestrator connection are
unready. A different readiness contract stays unavailable; the gateway does
not bypass it or submit speculatively.

Set the backend's file-size, page and conversion-time limits. The initial
pilot uses at most 100 MiB and 200 pages, with a finite timeout. Set
`DOCLING_SERVE_SINGLE_USE_RESULTS=false` so retrieval can be retried.
[Docling's configuration reference](https://github.com/docling-project/docling-serve/blob/main/docs/configuration.md)
names these settings.

Give Docling's scratch directory a separate finite storage allocation. Its
retained results and expanded Office or audio intermediates are outside the
gateway's spool budget. Stop Docling before cleaning its scratch; first check
that the gateway retained the results you need. A thread setting does not
limit process memory, GPU memory, or disk use.

## Build and start the gateway

Build from the source checkout:

```sh
go build -tags fts5 -o bin/docbank-conversion-worker ./cmd/docbank-conversion-worker
```

Choose a private, dedicated directory outside any vault. Supply keys through
the process environment, using your normal secret-management mechanism:

```sh
bin/docbank-conversion-worker \
  --directory /private/path/conversion-spool \
  --backend http://127.0.0.1:5001 \
  --listen 127.0.0.1:5002
```

`DOCBANK_WORKER_API_KEY` and `DOCBANK_WORKER_BACKEND_KEY` are required.
The default listener is loopback. To allow a vault host on an operator-controlled
trusted network, bind an explicit private or shared-network IP and restrict
access at the network boundary. Use encrypted transport when the network is
not trusted. Never expose an authenticated plaintext listener to the public
Internet.

The worker refuses proxies, redirects, non-loopback backend addresses, and a
second owner of the same spool, including a filesystem alias. It rejects its
own listener as a backend. Backend requests carry a hop marker; another
gateway refuses them before forwarding, so gateways cannot form readiness
loops. Point the backend directly at Docling Serve. Retained jobs bind
the backend origin; use a separate spool when changing that origin. It has no vault
API, shared database, URL-input route, or arbitrary pipeline option.

## Route a Docbank profile to the worker

Choose an explicit worker rendition profile in Docbank's existing processing
configuration. These opt-in Docbank client integrations implement the
stable-key gateway contract. Use `docbank-worker-document/v1` for documents or
`docbank-worker-asr/v1` for original WAV/MP3. Both use descriptor ID
`docbank.conversion-worker-v1` and require `trust_boundary = "operator_network"`.
Native `docbank-docling-*` profiles keep their native endpoint contract. The
audio adapter expects the qualified Docling Serve `1.32.0` / document schema
`1.10.0` response contract; qualify later versions with synthetic WAV/MP3
before use. Worker profiles reject password-decrypted PDFs before network
access because decrypted bytes are not deterministic across retries.

Construct document descriptors with `docling.WorkerDocumentDescriptor()` and
audio descriptors with `docling.WorkerASRDescriptor(maxTranscriptChars)` from
`go.kenn.io/docbank/document/docling`. Copy the returned fingerprint into
`descriptor_fingerprint`. Compute `disclosure_fingerprint` with
`docling.WorkerDisclosureFingerprint(adapterContract, descriptor, endpoint,
deploymentFingerprint)`. Changing the endpoint or deployment requires a new
disclosure and renewed consent. The audio descriptor also binds the transcript
character limit.

Use the ordinary rendition `runtime` block with the gateway's root origin,
finite request/polling timeouts, explicit private/shared/loopback `allowed_cidrs`
and `proxy_mode = "disabled"`. Bind `credential:<name>` to an environment
variable containing the gateway key. Give Docling's separate backend key only
to the worker. Keep source and response limits at or below 100 MiB and 128 MiB;
document `max_units` is at most 200. Request `provider_markdown` for documents,
optionally `structured_evidence`; audio requests only `provider_transcript`. See
[processing configuration](configuration.md) for the surrounding profile graph.

The existing processing plan shows the worker destination and requires consent
before sending bytes. Selecting a worker profile is opt-in. Keep cheap text on
the local profile and use a supplied transcript when one is available. If the
worker is unavailable, its durable rendition job waits and retries. It does
not fall back to heavy conversion on the vault host or to a hosted provider.

After durable acceptance, a checkpoint stores the issued operation and the
original authorization facts. A known operation resumes without reopening or
resending the original, including after the original authorization interval
expires. If the daemon dies before saving its checkpoint, the exact worker
profile can recover after lease expiry: it rechecks current consent and runtime,
reopens the exact source, and looks up the same immutable request key before
new-work readiness. Expired authority is never extended in an existing receipt.
Receipt timestamps record acceptance or revalidation within that authority,
not physical model completion or retrieval latency. Resume keeps those facts;
request/retry usage counts only the current invocation. Epoch and consent
fences still govern publication. Other adapters retain their
existing ambiguous-submission handling.

Rendition publication and embedding are independent. Text becomes searchable
when its lexical generation is published. Embedding outages leave their own
jobs pending; recovering those jobs does not reconvert the document. Original
and derivative retention follow the selected processing profile, independently
of either worker's completion.

## Storage and recovery

The defaults are 64 MiB per input, 64 MiB per result, 128 retained jobs and a
40 GiB gateway spool reservation budget. The hard pilot input/result limits
are 100 MiB and 128 MiB. Admission reserves space for source, result staging
and bounded metadata before receiving a new job. One upload and one backend
conversion run at a time. Polling and retained result reads remain available
while an upload is in progress.

The worker acknowledges input only after committing it to the spool. A stable
request key returns the same issued job ID after a connection loss or restart.
A key reused for different bytes or options is rejected. Results are retained
before success is reported, and reading a result does not delete it. Lookup
and result recovery work when the spool is full or Docling is offline.

A confirmed missing Docling task can be recomputed from retained input. The
worker persists a lifetime limit of three accepted or possibly accepted
backend submissions per job. A definite HTTP 429 refusal releases its reserved
attempt and waits. An ambiguous transport or server failure keeps it charged.
After exhaustion, the job fails and retains its input for operator inspection.
Crashes can repeat compute; publication in Docbank remains separately fenced.

There is no automatic expiry or garbage collection. At the storage or job
ceiling, new jobs wait. Stop the worker and verify publication in Docbank
before removing a completed job directory. Keep pending, failed, or unacknowledged
outputs until their disposition is clear. Never delete originals to free the
worker spool.

Shutdown drains or cancels active HTTP handlers and joins the conversion runner
before releasing spool ownership. File publication uses the repository's
portable atomic-file and private-file primitives. A durability error stops
admission rather than reporting success; reopen the spool to reconcile it.
Power-loss guarantees inherit the filesystem and platform's directory-sync
semantics.

## Protocol and pilot boundary

Every route requires `X-Api-Key`. Upload uses
`POST /v1/convert/file/async`, a single `files` multipart part, a 64-character
lowercase hexadecimal `Idempotency-Key`, and the exact input digest in
`X-Docbank-Source-SHA256`. Only fixed in-body document or audio conversion
options are accepted. The gateway returns a random 32-character hexadecimal
task ID. `GET /v1/jobs/by-key/<key>` recovers that ID without contacting Docling.
`GET /v1/status/poll/<id>` returns the worker lifecycle `task_status`. After
Docling completes, `backend_task_status` preserves Docling `success` or
`partial_success`; `partial_success` remains readable from
`GET /v1/result/<id>`. `GET /ready` checks backend readiness for new work.
Existing-key recovery precedes readiness.

The gateway does not select or process vault files automatically. A client must
implement the stable-key protocol. An ordinary native Docling endpoint does
not provide these durable replay guarantees. Gateways accept only the listed
fixed document or audio upload options; upstream support for richer pipelines
does not enable them here.

The protocol accepts PDF, PPTX, XLSX, PNG/JPEG, text/Markdown and WAV/MP3 content
types. Clients remain responsible for local eligibility inspection, exact
source authorization and qualified evidence mapping. Keep cheap text local
and prefer an available supplied transcript. Password-decrypted PDFs require
a separate deterministic-input contract before they can be retried safely.

Multi-gigabyte transfer, video demux, M4A/MP4/MKV, segmented long recordings,
video understanding, automatic profile selection and automatic cleanup are
outside this pilot. Retention is separate from conversion or embedding
completion.
