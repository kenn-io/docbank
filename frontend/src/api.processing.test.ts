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
            `${JSON.stringify({ sequence: 1, type: "job", job: { id: jobID, embedding_job_ids: [], profile_fingerprint: fingerprint, content_version_id: versionID } })}\n${JSON.stringify({ sequence: 2, type: "status", status: { job_id: jobID, state: "completed", phase: "complete", embedding_job_ids: [], completed_bindings: 0 }, terminal: true })}\n`,
            { headers: { "Content-Type": "application/x-ndjson" } },
          );
        }
        if (path.startsWith("/api/v1/coverage?")) {
          return Response.json({ vault_uid: versionID, profile_fingerprint: fingerprint, state: "complete", renditions: { name: "rendition", required: true, state: "complete", complete: 1, unavailable: 0, stale: 0, ineligible: 0, rebuilding: 0, previous_generation_serving: 0, total: 1 }, embeddings: [] });
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
    await expect(startProcessing("session", { node_id: 42, content_version_id: versionID, profile: "private" }, fingerprint, true)).resolves.toMatchObject({ status: { state: "completed" } });
    await expect(documentCoverage("session", "private", versionID, [versionID])).resolves.toMatchObject({ state: "complete" });
    await expect(documentSearch("session", { query: "synthetic", mode: "auto", limit: 20, profile: "private", fence: { vault_uid: versionID, content_version_ids: [versionID] }, explain: true })).resolves.toMatchObject({ actual_mode: "lexical" });
    expect(fetchMock).toHaveBeenCalledTimes(5);
  });

  it("delivers the durable job before a blocked terminal status", async () => {
    const versionID = "11111111-1111-4111-8111-111111111111";
    const fingerprint = "a".repeat(64);
    const jobID = "b".repeat(64);
    const encoder = new TextEncoder();
    let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(new ReadableStream<Uint8Array>({
      start(controller) {
        streamController = controller;
        controller.enqueue(encoder.encode(`${JSON.stringify({
          sequence: 1,
          type: "job",
          job: { id: jobID, embedding_job_ids: [], profile_fingerprint: fingerprint, content_version_id: versionID },
        })}\n`));
      },
    }), { headers: { "Content-Type": "application/x-ndjson" } }));
    let releaseObserved!: () => void;
    const observed = new Promise<void>((resolve) => { releaseObserved = resolve; });
    let settled = false;
    const pending = startProcessing(
      "session",
      { node_id: 42, content_version_id: versionID, profile: "private" },
      fingerprint,
      true,
      (event) => {
        if (event.type === "job") {
          expect(event.job?.id).toBe(jobID);
          releaseObserved();
        }
      },
    ).finally(() => { settled = true; });

    await observed;
    expect(settled).toBe(false);
    streamController?.enqueue(encoder.encode(`${JSON.stringify({
      sequence: 2,
      type: "status",
      status: { job_id: jobID, state: "completed", phase: "published", embedding_job_ids: [], completed_bindings: 0 },
      terminal: true,
    })}\n`));
    streamController?.close();
    await expect(pending).resolves.toMatchObject({ job: { id: jobID }, status: { state: "completed" } });
  });

  it("rejects malformed, trailing, and oversized incremental progress", async () => {
    const jobID = "b".repeat(64);
    const job = `${JSON.stringify({ sequence: 1, type: "job", job: {
      id: jobID,
      embedding_job_ids: [],
      profile_fingerprint: "a".repeat(64),
      content_version_id: "11111111-1111-4111-8111-111111111111",
    } })}\n`;
    const terminal = `${JSON.stringify({ sequence: 2, type: "status", status: {
      job_id: jobID,
      state: "completed",
      phase: "published",
      embedding_job_ids: [],
      completed_bindings: 0,
    }, terminal: true })}\n`;
    for (const [body, message] of [
      [job + terminal + `${JSON.stringify({ sequence: 3, type: "status" })}\n`, /did not end/i],
      [job + "x".repeat(256 * 1024), /too large/i],
    ] as const) {
      vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(new Response(body, {
        headers: { "Content-Type": "application/x-ndjson" },
      }));
      await expect(startProcessing("session", {
        node_id: 42,
        content_version_id: "11111111-1111-4111-8111-111111111111",
        profile: "private",
      }, "a".repeat(64), true)).rejects.toThrow(message);
    }
  });

  it("propagates cancellation to the processing request", async () => {
    const controller = new AbortController();
    let receivedSignal: AbortSignal | null | undefined;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) => {
      receivedSignal = init?.signal;
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
      });
    });
    const pending = startProcessing("session", {
      node_id: 42,
      content_version_id: "11111111-1111-4111-8111-111111111111",
      profile: "private",
    }, "a".repeat(64), true, undefined, controller.signal);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    expect(receivedSignal).toBe(controller.signal);
  });

  it("rejects search results outside the fence and malformed result identities", async () => {
    const vaultID = "11111111-1111-4111-8111-111111111111";
    const versionID = "22222222-2222-4222-8222-222222222222";
    const otherVersionID = "55555555-5555-4555-8555-555555555555";
    const request = { query: "visible", mode: "semantic" as const, limit: 20, profile: "private",
      fence: { vault_uid: vaultID, content_version_ids: [versionID, otherVersionID] } };
    const valid = () => ({
      requested_mode: "semantic", actual_mode: "semantic",
      coverage: { binding_required: false, scoped_documents: 1, complete_documents: 1, state: "complete" },
      degradations: [], truncated: false, trace: [],
      results: [{ vault_uid: vaultID, node_id: 7, content_version_id: versionID, rank: 1,
        semantic_rank: 1, score: 0.9, path: "/visible.pdf", excerpt: "Synthetic match", evidence: [{
          kind: "embedding", vector_space_id: "a".repeat(64), embedding_set_id: "b".repeat(64),
          input_generation_id: "c".repeat(64), input_id: "chunk-000000-aaaaaaaaaaaa",
          input_kind: "rendition_chunk", source_manifest_checksum: "d".repeat(64),
        }] }],
    });
    const malformed = [
      (report: ReturnType<typeof valid>) => { report.results[0]!.vault_uid = "33333333-3333-4333-8333-333333333333"; },
      (report: ReturnType<typeof valid>) => { report.results[0]!.content_version_id = "44444444-4444-4444-8444-444444444444"; },
      (report: ReturnType<typeof valid>) => { report.results.push({ ...report.results[0]!, rank: 2, semantic_rank: 2 }); },
      (report: ReturnType<typeof valid>) => { report.results[0]!.rank = 2; },
      (report: ReturnType<typeof valid>) => { report.results[0]!.semantic_rank = 1001; },
      (report: ReturnType<typeof valid>) => { report.results.push({ ...report.results[0]!, node_id: 8,
        content_version_id: otherVersionID, rank: 2,
        evidence: [{ ...report.results[0]!.evidence[0]!, embedding_set_id: "e".repeat(64) }] }); },
      (report: ReturnType<typeof valid>) => { report.results[0]!.evidence.push({ ...report.results[0]!.evidence[0]! }); },
      (report: ReturnType<typeof valid>) => { report.results[0]!.evidence[0]!.input_generation_id = ""; },
      (report: ReturnType<typeof valid>) => { report.results[0]!.path = "visible.pdf"; },
      (report: ReturnType<typeof valid>) => { report.results[0]!.path = `/${"p".repeat((16 * 1024) + 1)}`; },
      (report: ReturnType<typeof valid>) => { report.results[0]!.excerpt = "e".repeat(513); },
    ];
    const fetchMock = vi.spyOn(globalThis, "fetch");
    for (const mutate of malformed) {
      const report = valid();
      mutate(report);
      fetchMock.mockResolvedValueOnce(Response.json(report));
      await expect(documentSearch("session", request)).rejects.toThrow(/search response/i);
    }
    fetchMock.mockResolvedValueOnce(Response.json(valid()));
    await expect(documentSearch("session", request)).resolves.toMatchObject({
      results: [{ content_version_id: versionID }],
    });
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

  it("cancels an oversized rendition before buffering past its declared bound", async () => {
    const attachmentID = "a".repeat(64);
    let pulls = 0;
    let cancelled = false;
    let delivered = 0;
    const body = new ReadableStream({
      type: "bytes",
      pull(controller) {
        pulls += 1;
        if (controller.byobRequest) {
          const view = controller.byobRequest.view as Uint8Array;
          view.fill(0);
          delivered += view.byteLength;
          controller.byobRequest.respond(view.byteLength);
          return;
        }
        const oversized = new Uint8Array(64 * 1024 * 1024 + 1);
        delivered += oversized.byteLength;
        controller.enqueue(oversized);
      },
      cancel() {
        cancelled = true;
      },
    }, { highWaterMark: 0 });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, {
      headers: {
        "Content-Type": "text/markdown; charset=utf-8",
        "X-Docbank-Rendition-Attachment": attachmentID,
        "X-Docbank-Rendition-Build": "b".repeat(64),
        "X-Docbank-Rendition-Artifact": "c".repeat(64),
        "X-Docbank-Content-Version": "11111111-1111-4111-8111-111111111111",
        "X-Docbank-Rendition-Profile": "d".repeat(64),
        "X-Docbank-Rendition-Completeness": "complete",
        "X-Docbank-Rendition-Warnings": "",
        "X-Docbank-Blob-Hash": "e".repeat(64),
        "X-Docbank-Blob-Size": "3",
      },
    }));

    await expect(renditionArtifact("session", attachmentID)).rejects.toThrow(/rendition size/i);
    expect(cancelled).toBe(true);
    expect(pulls).toBe(1);
    expect(delivered).toBe(4);
  });
});
