// Package gcs stores immutable bytes in Google Cloud Storage through the
// official Go client and its JSON API.
//
// Config requires explicit OAuth credentials. The store never discovers
// credentials itself; callers choose a source such as
// credentials.DetectDefault from cloud.google.com/go/auth/credentials, which
// supports workload identity on Google Cloud. Construction makes no requests.
//
// Publish uploads with a does-not-exist precondition and sends the validated
// CRC32C, so GCS rejects bytes that differ from the source. A normal receipt
// uses the stored generation, length, and CRC32C from the upload response.
// Existing keys and ambiguous completion require generation-guarded full
// SHA-256 readback. Failed calls return a zero receipt. ErrOutcomeUnknown
// requires the caller to retain its intent and reconcile; it does not
// authorize deletion. Source readers remain caller-owned and must stay stable
// until Publish returns. A single-request upload, used up to 16 MiB, retries
// transient failures until the caller's context ends, so callers should set
// a deadline. A larger resumable upload retries each chunk for at most the
// client's 32-second default. Other requests make at most three attempts.
//
// Revision is the object's opaque generation. OpenRange and Delete honor a
// supplied generation precondition. Inventory includes foreign names and
// grants no cleanup authority. Tests run against an in-process
// fake-gcs-server, which does not establish real GCS compatibility.
package gcs
