import { afterEach, describe, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import {
  documentCoverage,
  documentSearch,
  processingPlan,
  processingProfiles,
  renditionArtifact,
  startProcessing,
} from "./api.js";

afterEach(() => vi.restoreAllMocks());

describe("document processing browser API", () => {
  it("retains final job identities when the stream reports unavailable status", async () => {
    const job = { id: "a".repeat(64), embedding_job_ids: [], profile_fingerprint: "b".repeat(64), content_version_id: "11111111-1111-4111-8111-111111111111" };
    const completedJob = { ...job, embedding_job_ids: ["c".repeat(64)] };
    const events = `${JSON.stringify({ sequence: 1, type: "job", job })}\n${JSON.stringify({
      sequence: 2, type: "error", terminal: true, job: completedJob,
      error: { status: 503, code: "processing_status_unavailable", detail: "Check the job status." },
    })}`;
    const body = new ReadableStream<Uint8Array>({ start(controller) {
      for (const part of [events.slice(0, 13), events.slice(13, -7), events.slice(-7)]) {
        controller.enqueue(new TextEncoder().encode(part));
      }
      controller.close();
    } });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "application/x-ndjson" } }));
    const onJob = vi.fn();
    await expect(startProcessing("session", { node_id: 42, content_version_id: job.content_version_id, profile: "private" }, job.profile_fingerprint, true, onJob)).rejects.toMatchObject({ status: 503, code: "processing_status_unavailable" });
    expect(onJob).toHaveBeenLastCalledWith(completedJob);
  });

  it("validates navigation across a large multilingual rendition", async () => {
    const chunk = "# α\n" + "x".repeat(10480) + "\n";
    const body = chunk.repeat(500);
    const entries = Array.from({ length: 500 }, (_, index) => ({
      key: `page:${index + 1}`, kind: "page", line: 1 + index * 2, byte: index * utf8ToBytes(chunk).length,
    }));
    const attachmentID = "a".repeat(64), buildID = "b".repeat(64);
    const metadata = { docbank: {
      contract: "docbank-sanitized-markdown/v1",
      source: { sha256: "c".repeat(64), format: "txt", media_type: "text/plain" },
      rendition: { build_id: buildID, rendition_request_fingerprint: "d".repeat(64), evidence_lexical_fingerprint: "e".repeat(64),
        normalized_evidence_contract: "normalized-evidence/v1", body_sha256: bytesToHex(sha256(utf8ToBytes(body))), completeness: "complete", truncated: false },
      document: { unit_kind: "page", unit_count: entries.length },
      navigation: { offset_base: "body", complete: true, entries },
    } };
    const artifact = `---\n${JSON.stringify(metadata)}\n---\n${body}`;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(artifact, { headers: {
      "Content-Type": "text/markdown", "X-Docbank-Rendition-Attachment": attachmentID,
      "X-Docbank-Rendition-Build": buildID, "X-Docbank-Rendition-Artifact": "f".repeat(64),
      "X-Docbank-Content-Version": "11111111-1111-4111-8111-111111111111",
      "X-Docbank-Rendition-Profile": "d".repeat(64), "X-Docbank-Rendition-Completeness": "complete",
      "X-Docbank-Rendition-Warnings": "", "X-Docbank-Blob-Hash": bytesToHex(sha256(utf8ToBytes(artifact))),
      "X-Docbank-Blob-Size": String(utf8ToBytes(artifact).length),
    } }));
    const result = await renditionArtifact("session", attachmentID);
    expect(result.navigation.entries).toEqual(entries.map((entry) => ({ ...entry, title: "" })));
  });

  it("keeps plans, jobs, coverage, and search on exact identities", async () => {
    const versionID = "11111111-1111-4111-8111-111111111111";
    const fingerprint = "a".repeat(64);
    const jobID = "b".repeat(64);
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
      async (input, request) => {
        const path = String(input);
        if (path === "/api/v1/processing/profiles") {
          return Response.json([{ name: "private", fingerprint, rendition: true, embedding_bindings: [] }]);
        }
        if (path === "/api/v1/processing/plans") {
          expect(request?.method).toBe("POST");
          expect(JSON.parse(String(request?.body))).toMatchObject({
            selector: { node_id: 42, content_version_id: versionID, profile: "private" },
          });
          return Response.json({ fingerprint, selector: { node_id: 42, content_version_id: versionID, profile: "private" }, flow: [], disclosed_classes: [], retained_classes: [], estimate: {}, consent_required: true, consent_state: "required" });
        }
        if (path === "/api/v1/processing/jobs") {
          expect(JSON.parse(String(request?.body))).toMatchObject({ consent: true });
          return new Response(
            `${JSON.stringify({ sequence: 1, type: "job", job: { id: jobID, embedding_job_ids: [], profile_fingerprint: fingerprint, content_version_id: versionID } })}\n${JSON.stringify({ sequence: 2, type: "status", status: { job_id: jobID, state: "completed", phase: "embedding", embedding_job_ids: [fingerprint], completed_bindings: 1 }, terminal: true })}\n`,
            { headers: { "Content-Type": "application/x-ndjson" } },
          );
        }
        if (path.startsWith("/api/v1/coverage?")) {
          return Response.json({ vault_uid: versionID, profile_fingerprint: fingerprint, state: "complete", renditions: { name: "rendition", required: true, state: "complete", complete: 1, unavailable: 0, stale: 0, ineligible: 0, total: 1 }, embeddings: [] });
        }
        if (path === "/api/v1/search") {
          expect(request?.method).toBe("POST");
          return Response.json({ requested_mode: "auto", actual_mode: "lexical", coverage: { binding_required: false, scoped_documents: 1, complete_documents: 1, state: "complete" }, degradations: [], results: [], truncated: false, trace: [] });
        }
        throw new Error(`unexpected request: ${path}`);
      },
    );

    await expect(processingProfiles("session")).resolves.toHaveLength(1);
    await expect(processingPlan("session", { node_id: 42, content_version_id: versionID, profile: "private" })).resolves.toMatchObject({ fingerprint });
    await expect(startProcessing("session", { node_id: 42, content_version_id: versionID, profile: "private" }, fingerprint, true)).resolves.toMatchObject({ status: { state: "completed" }, job: { embedding_job_ids: [fingerprint] } });
    await expect(documentCoverage("session", "private", versionID, [versionID])).resolves.toMatchObject({ state: "complete" });
    await expect(documentSearch("session", { query: "synthetic", mode: "auto", limit: 20, profile: "private", fence: { vault_uid: versionID, content_version_ids: [versionID] }, explain: true })).resolves.toMatchObject({ actual_mode: "lexical" });
    expect(fetchMock).toHaveBeenCalledTimes(5);
  });

  it("rejects a rendition whose retained body checksum does not match", async () => {
    const attachmentID = "a".repeat(64);
    const buildID = "b".repeat(64);
    const artifact = `---\ndocbank:\n  contract: "docbank-sanitized-markdown/v1"\n  source:\n    sha256: "${"e".repeat(64)}"\n    format: "pdf"\n    media_type: "application/pdf"\n  rendition:\n    build_id: "${buildID}"\n    rendition_request_fingerprint: "${"d".repeat(64)}"\n    evidence_lexical_fingerprint: "${"f".repeat(64)}"\n    normalized_evidence_contract: "normalized-evidence/v1"\n    body_sha256: "${"0".repeat(64)}"\n    completeness: "complete"\n    truncated: false\n  document:\n    unit_kind: "page"\n    unit_count: 1\n  navigation:\n    offset_base: "body"\n    complete: true\n    entries:\n      - key: "page:1"\n        kind: "page"\n        line: 1\n        byte: 0\n---\n# Report\n`;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(artifact, {
        headers: {
          "Content-Type": "text/markdown; charset=utf-8",
          "X-Docbank-Rendition-Attachment": attachmentID,
          "X-Docbank-Rendition-Build": buildID,
          "X-Docbank-Rendition-Artifact": "c".repeat(64),
          "X-Docbank-Content-Version": "11111111-1111-4111-8111-111111111111",
          "X-Docbank-Rendition-Profile": "d".repeat(64),
          "X-Docbank-Rendition-Completeness": "complete",
          "X-Docbank-Rendition-Warnings": "",
          "X-Docbank-Blob-Hash": bytesToHex(sha256(utf8ToBytes(artifact))),
          "X-Docbank-Blob-Size": String(artifact.length),
        },
      }),
    );
    await expect(renditionArtifact("session", attachmentID)).rejects.toThrow(/body checksum/i);
  });
});
