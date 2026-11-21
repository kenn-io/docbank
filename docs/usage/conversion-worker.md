---
title: Run a bounded conversion worker
description: Keep conversion off the vault host with a private durable spool, bounded Docling jobs, and repeatable result retrieval.
---

# Run a bounded conversion worker

A conversion worker moves document conversion and speech transcription off the
vault host while originals stay in Docbank. The worker keeps temporary inputs
and results in its own private directory and sends conversion requests to a
separately installed Docling Serve on the same machine.

This worker is an opt-in operator service. Building it does not install, start,
or configure a service, change authentication, or keep a computer awake. You
can run it on an Apple Silicon Mac, and having `uv` and `ffmpeg` installed does
not qualify a Docling model or its output.

## Prepare the backend

Run an exclusive Docling Serve deployment bound to a numeric loopback address.
Set `UVICORN_WORKERS=1`, `DOCLING_SERVE_ENG_LOC_NUM_WORKERS=1`, and
`DOCLING_NUM_THREADS=4`. The gateway serializes backend requests, and the
backend's single conversion worker prevents an accepted request whose response
was lost from running alongside its replacement.

Keep remote services and external plugins off by setting
`DOCLING_SERVE_ENABLE_REMOTE_SERVICES=false` and
`DOCLING_SERVE_ALLOW_EXTERNAL_PLUGINS=false`. Qualify the exact document and
ASR versions before you process documents, and use separate API keys for
Docbank's worker connection and the loopback backend.

The backend must answer `GET /ready` with HTTP 200 and
`Content-Type: application/json` when ready. The
[qualified Docling Serve 1.32.0 route](https://github.com/docling-project/docling-serve/blob/v1.32.0/docling_serve/app.py#L732)
returns 503 while its models, queue processor, or orchestrator connection are
unready. A different readiness contract stays unavailable, and the gateway does
not bypass it or submit speculatively.

Configure the backend's file-size, page, and conversion-time limits. The
initial pilot uses at most 100 MiB and 200 pages, with a finite timeout. Set
`DOCLING_SERVE_SINGLE_USE_RESULTS=false` so retrieval can be retried. For the
exact setting names, see
[Docling's configuration reference](https://github.com/docling-project/docling-serve/blob/main/docs/configuration.md).

Give Docling's scratch directory a separate finite storage allocation. Its
retained results and expanded Office or audio intermediates sit outside the
gateway's spool budget. Before you clean Docling's scratch directory, check
that the gateway retained the results you need, then stop Docling. A thread
setting does not limit process memory, GPU memory, or disk use.

## Build and start the gateway

Build the binary from the source checkout:

```sh
go build -tags fts5 -o bin/docbank-conversion-worker ./cmd/docbank-conversion-worker
```

Choose a private, dedicated directory outside any vault, and supply the keys
through the process environment with your normal secret-management tool:

```sh
bin/docbank-conversion-worker \
  --directory /private/path/conversion-spool \
  --backend http://127.0.0.1:5001 \
  --listen 127.0.0.1:5002
```

Both `DOCBANK_WORKER_API_KEY` and `DOCBANK_WORKER_BACKEND_KEY` are required.
The default listener is loopback. To allow a vault host on an
operator-controlled trusted network, bind an explicit private or shared-network
IP and restrict access at the network boundary. If the network is not trusted,
use encrypted transport. Never expose an authenticated plaintext listener to
the public Internet.

The worker refuses proxies, redirects, non-loopback backend addresses, and a
second owner of the same spool, including a filesystem alias. It also rejects
its own listener as a backend. Backend requests carry a hop marker that another
gateway refuses before forwarding, so gateways cannot form readiness loops.
Point the backend directly at Docling Serve. Retained jobs bind the backend
origin. If you change that origin, use a separate spool. The worker has no
vault API, shared database, URL-input route, or arbitrary pipeline option.

## Storage and recovery

By default, the gateway allows 64 MiB per input, 64 MiB per result, 128
retained jobs, and a 40 GiB spool reservation budget. The hard pilot limits are
100 MiB for input and 128 MiB for results. Before receiving a new job,
admission reserves space for the source, result staging, and bounded metadata.
One upload and one backend conversion run at a time, while polling and retained
result reads stay available during an upload. If you lower a limit, accepted
jobs keep their original byte limits and reserved capacity, and new jobs wait
until the spool is within the configured limits.

The worker acknowledges input only after committing it to the spool. After a
connection loss or restart, a stable request key returns the same issued job
ID, while reusing a key for different bytes or options is rejected. The worker
retains results before reporting success, and reading a result does not delete
it. Lookup and result recovery work even when the spool is full or Docling is
offline.

The worker can recompute a confirmed missing Docling task from its retained
input. It allows up to three accepted or possibly accepted backend submissions
per retry cycle. A definite HTTP 429 refusal releases its reserved attempt and
waits, while an ambiguous transport or server failure keeps the attempt
charged. Once the limit is exhausted, the job fails and retains its input. If
the backend returns HTTP 401, you can retry after correcting the backend key.
To retry any failure marked `retryable`, repeat the upload with the same key,
source, and options. The worker keeps the job ID and resets the submission
count. Other permanent backend refusals and failed backend tasks stay failed.
After two hours, a started backend task no longer holds up pending jobs, and
the worker rotates through stalled tasks and pending jobs, including when the
backend refuses new submissions. Crashes can repeat compute, and publication in
Docbank remains separately fenced.

The spool has no automatic expiry or garbage collection. When it hits the
storage or job ceiling, new jobs wait. Before you remove a completed job
directory, stop the worker and verify publication in Docbank. Keep pending,
failed, or unacknowledged outputs until their disposition is clear, and never
delete originals to free the worker spool.

On shutdown, the worker drains or cancels active HTTP handlers and joins the
conversion runner before releasing spool ownership. File publication uses the
repository's portable atomic-file and private-file operations. If a durability
error occurs, the worker stops admission rather than reporting success; reopen
the spool to reconcile it. Power-loss guarantees inherit the filesystem and
platform's directory-sync semantics.

## Protocol and pilot boundary

Every route requires `X-Api-Key`. Uploads use `POST /v1/convert/file/async`
with a single `files` multipart part, a 64-character lowercase hexadecimal
`Idempotency-Key`, and the exact input digest in `X-Docbank-Source-SHA256`.
The gateway accepts only fixed in-body document or audio conversion options and
returns a random 32-character hexadecimal task ID.

Use `GET /v1/jobs/by-key/<key>` to recover that ID without contacting Docling,
and `GET /v1/status/poll/<id>` to read the worker lifecycle `task_status`. Both
lookup and status responses include `retryable: true` for a recoverable
failure. After Docling finishes, `backend_task_status` preserves Docling
`success` or `partial_success`, and `partial_success` remains readable from
`GET /v1/result/<id>`. `GET /ready` checks backend readiness for new work, and
existing-key recovery runs ahead of readiness.

The gateway does not select or process vault files automatically. A client must
implement the stable-key protocol. An ordinary native Docling endpoint does not
provide these durable replay guarantees. Gateways accept only the listed fixed
document or audio upload options; upstream support for richer pipelines does
not enable them here.

The protocol accepts PDF, PPTX, XLSX, PNG/JPEG, and WAV/MP3 content types, with
audio limited to one hour. Clients remain responsible for local eligibility
inspection, exact source authorization, and qualified evidence mapping. Keep
cheap text local and prefer an available supplied transcript.
Password-decrypted PDFs require a separate deterministic-input contract before
you can retry them safely.

Multi-gigabyte transfer, video demux, M4A/MP4/MKV, segmented long recordings,
video understanding, automatic profile selection, and automatic cleanup are
outside this pilot. Retention is separate from conversion or embedding
completion.
