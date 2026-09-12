import { afterEach, describe, expect, it, vi } from "vitest";
import { prepareEmailPDFDownload } from "./download.js";
import { retainedEmailPDFs, renderEmailPDF, validatePDFReceipt, type EmailPDFReceipt } from "./email-pdf.js";
import type { ContentVersion, Node } from "./generated/docbank.js";

afterEach(() => vi.unstubAllGlobals());

describe("email PDF exact selection", () => {
  it("rejects a receipt for another original or recipe", () => {
    expect(() => validatePDFReceipt({ source: { version_id: "wrong" } }, "selected", "a".repeat(64))).toThrow("selected");
  });

  it("uses exact receipt authority across retained lookup, rendering, and browser download", async () => {
    const node: Node = {
      id: 7, parent_id: 2, name: "synthetic.eml", kind: "file", revision: 3,
      current_version_id: "12345678-1234-4123-8123-123456789abc",
      blob_hash: "a".repeat(64), size: 25, mime_type: "message/rfc822",
      created_at: "2026-07-26T12:00:00Z", modified_at: "2026-07-26T12:00:00Z",
    };
    const version: ContentVersion = {
      id: "abcdefab-cdef-4abc-8def-abcdefabcdef", node_id: node.id,
      blob_hash: "b".repeat(64), size: 12, mime_type: "message/rfc822",
      recorded_at: "2026-07-20T12:00:00Z", node_revision: 1,
      introduced_operation_id: "87654321-4321-4321-8321-cba987654321", transition_kind: "content_create",
    };
    const receipt: EmailPDFReceipt = {
      source: { node_id: node.id, version_id: version.id, sha256: version.blob_hash, size: version.size },
      attachment_id: "c".repeat(64), build_id: "d".repeat(64), profile_fingerprint: "e".repeat(64),
      binding: {
        generation_id: "f".repeat(64), generation_checksum: "1".repeat(64),
        recipe: {
          contract: "email-pdf-v1", renderer_version: "synthetic",
          renderer_sha256: "2".repeat(64), worker_sha256: "3".repeat(64), bubblewrap_sha256: "8".repeat(64), fonts_sha256: "4".repeat(64), paper: "A4",
        },
      },
      output: { body_path: "body.html", body_sha256: "5".repeat(64), body_size: 10, pdf_sha256: "6".repeat(64), pdf_size: 100, pages: 1 },
    };
    const jobID = "7".repeat(64);
    const ready = {
      phase: "ready", received: receipt.output.pdf_size, total: receipt.output.pdf_size,
      url: "/api/daemon/web-download/file?ticket=retained-pdf", name: "message.pdf",
      version_id: version.id, blob_hash: receipt.output.pdf_sha256,
    };
    const responses = [
      [receipt],
      { job_id: jobID, version_id: version.id, profile_fingerprint: receipt.profile_fingerprint, state: "queued" },
      { state: "completed" },
      receipt,
      ready,
    ];
    vi.stubGlobal("fetch", vi.fn().mockImplementation(async () => new Response(JSON.stringify(responses.shift()))));
    const signal = new AbortController().signal;

    await expect(retainedEmailPDFs("session", version.id, signal)).resolves.toEqual([receipt]);
    const rendered = await renderEmailPDF("session", version.id, "A4", signal);
    await expect(prepareEmailPDFDownload("session", node, version, rendered, signal, () => undefined)).resolves.toMatchObject({
      name: "message.pdf", versionID: version.id, blobHash: receipt.output.pdf_sha256, size: receipt.output.pdf_size,
    });
    expect(vi.mocked(fetch).mock.calls.map(([path]) => path)).toEqual([
      `/api/v1/email-pdfs/${version.id}`,
      "/api/v1/email-pdfs",
      `/api/v1/email-pdf-jobs/${jobID}`,
      `/api/v1/email-pdfs/${version.id}/${receipt.profile_fingerprint}`,
      "/api/daemon/web-download",
    ]);
    expect(JSON.parse(String(vi.mocked(fetch).mock.calls.at(-1)?.[1]?.body))).toEqual({
      node_id: node.id, revision: node.revision, version_id: version.id,
      blob_hash: receipt.output.pdf_sha256, size: receipt.output.pdf_size,
      email_pdf_profile: receipt.profile_fingerprint, email_pdf_attachment: receipt.attachment_id,
    });
  });
});
