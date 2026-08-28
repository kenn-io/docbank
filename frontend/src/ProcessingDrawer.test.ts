import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import ProcessingDrawer from "./ProcessingDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("document processing drawer", () => {
  it("shows reviewed flow, independent coverage, partial failure, and provenance", async () => {
    const versionID = "11111111-1111-4111-8111-111111111111";
    const vaultID = "22222222-2222-4222-8222-222222222222";
    const fingerprint = "a".repeat(64);
    const attachmentID = "b".repeat(64);
    let coverageReads = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const path = String(input);
      if (path === "/api/v1/processing/profiles") {
        return Response.json([{ name: "private", fingerprint, rendition: true, embedding_bindings: ["semantic"] }]);
      }
      if (path === "/api/v1/processing/plans") {
        return Response.json({
          fingerprint,
          vault_uid: vaultID,
          selector: { node_id: 42, content_version_id: versionID, profile: "private" },
          profile_fingerprint: fingerprint,
          flow: [
            { capability: "rendition", provider_id: "docling-local", trust_boundary: "operator_network", input_classes: ["original_file"] },
            { capability: "embedding", provider_id: "local-embed", trust_boundary: "local_process", input_classes: ["rendition_chunk"] },
          ],
          disclosed_classes: ["original_file", "rendition_chunk"],
          retained_classes: ["sanitized_markdown", "embedding_vector_set"],
          estimate: { source_bytes: 2048, provider_calls: 2, vector_spaces: 1 },
          consent_required: true,
          consent_state: "required",
          backup_consequence: "retained derivatives enter future backups",
        });
      }
      if (path.startsWith("/api/v1/coverage?")) {
        coverageReads += 1;
        return Response.json({
          vault_uid: vaultID,
          profile_fingerprint: fingerprint,
          state: coverageReads === 1 ? "missing" : "partial",
          renditions: { name: "rendition", required: true, state: coverageReads === 1 ? "missing" : "complete", complete: coverageReads === 1 ? 0 : 1, unavailable: 0, stale: 0, ineligible: 0, total: 1 },
          embeddings: [{ name: "semantic", required: false, state: "unavailable", complete: 0, unavailable: 1, stale: 0, ineligible: 0, total: 1 }],
        });
      }
      if (path === "/api/v1/processing/jobs") {
        const jobID = "c".repeat(64);
        return new Response(
          `${JSON.stringify({ sequence: 1, type: "job", job: { id: jobID, attachment_id: attachmentID, embedding_job_ids: ["d".repeat(64)], profile_fingerprint: fingerprint, content_version_id: versionID } })}\n${JSON.stringify({ sequence: 2, type: "status", status: { job_id: jobID, state: "partial", phase: "embedding", failure_code: "provider_unavailable", embedding_job_ids: ["d".repeat(64)], completed_bindings: 0 }, terminal: true })}\n`,
          { headers: { "Content-Type": "application/x-ndjson" } },
        );
      }
      if (path === "/api/v1/search") {
        return Response.json({
          requested_mode: "auto", actual_mode: "lexical",
          coverage: { binding_required: false, scoped_documents: 1, complete_documents: 1, state: "complete" },
          degradations: ["semantic_unavailable"],
          results: [{ vault_uid: vaultID, node_id: 42, content_version_id: versionID, rank: 1, score: 1, path: "/Reports/report.pdf", excerpt: "Synthetic match", evidence: [{ kind: "lexical_segment", build_id: "e".repeat(64) }] }],
          truncated: false, trace: [{ code: "source_fence", count: 1 }],
        });
      }
      throw new Error(`unexpected request: ${path}`);
    });
    const openRendition = vi.fn();

    render(ProcessingDrawer, {
      session: "short-lived",
      node: { id: 42, name: "report.pdf", kind: "file", current_version_id: versionID, size: 2048, revision: 1, created_at: "", modified_at: "" },
      path: "/Reports/report.pdf",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
      onrendition: openRendition,
    });

    expect(await screen.findByText("docling-local")).toBeTruthy();
    expect(screen.getAllByText(fingerprint).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Private network").length).toBeGreaterThan(0);
    expect(screen.getByText("Local process")).toBeTruthy();
    expect(screen.getByText(/processing profile and operator scope until revoked or expired/i)).toBeTruthy();
    expect(screen.getByText(/retained sanitized Markdown/i)).toBeTruthy();
    expect(screen.getByText(/2 provider calls/i)).toBeTruthy();
    expect(screen.getByText(/1 vector space/i)).toBeTruthy();
    expect(screen.getByText(/retained derivatives enter future backups/i)).toBeTruthy();
    expect(screen.getByText(/original_file.*rendition_chunk/i)).toBeTruthy();
    expect(screen.getByText(/sanitized_markdown.*embedding_vector_set/i)).toBeTruthy();
    expect(await screen.findByText(/rendition.*missing/i)).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Consent and run" }));
    expect(await screen.findByText(/provider_unavailable/i)).toBeTruthy();
    expect(await screen.findByText(/rendition.*complete/i)).toBeTruthy();
    expect(screen.getByText(/semantic.*unavailable/i)).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Read sanitized Markdown" }));
    expect(openRendition).toHaveBeenCalledWith(attachmentID);

    await fireEvent.input(screen.getByRole("searchbox", { name: "Search this document version" }), { target: { value: "synthetic" } });
    await fireEvent.click(screen.getByRole("button", { name: "Search this version" }));
    expect(await screen.findByText("Synthetic match")).toBeTruthy();
    expect(screen.getByText(/lexical_segment/i)).toBeTruthy();
    expect(screen.getByText(/semantic_unavailable/i)).toBeTruthy();
  });

  it("keeps hosted disclosure, required failure, rebuild fallback, and direct-file evidence explicit", async () => {
    const versionID = "11111111-1111-4111-8111-111111111111";
    const vaultID = "22222222-2222-4222-8222-222222222222";
    const fingerprint = "a".repeat(64);
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const path = String(input);
      if (path === "/api/v1/processing/profiles") {
        return Response.json([{ name: "hosted", fingerprint, rendition: false, embedding_bindings: ["direct-file"] }]);
      }
      if (path === "/api/v1/processing/plans") {
        return Response.json({
          fingerprint,
          vault_uid: vaultID,
          selector: { node_id: 42, content_version_id: versionID, profile: "hosted" },
          profile_fingerprint: fingerprint,
          flow: [{ capability: "embedding", provider_id: "gemini-file", trust_boundary: "hosted_provider", input_classes: ["original_file"] }],
          disclosed_classes: ["original_file"], retained_classes: ["embedding_vector_set"],
          estimate: { source_bytes: 2048, provider_calls: 1, vector_spaces: 1 },
          consent_required: true, consent_state: "revoked", backup_consequence: "vectors enter future backups",
        });
      }
      if (path.startsWith("/api/v1/coverage?")) {
        return Response.json({
          vault_uid: vaultID, profile_fingerprint: fingerprint, state: "rebuilding",
          renditions: { name: "rendition", required: false, state: "ineligible", complete: 0, unavailable: 0, stale: 0, ineligible: 1, total: 1 },
          embeddings: [{ name: "direct-file", required: true, state: "unavailable", complete: 0, unavailable: 1, stale: 0, ineligible: 0, total: 1 }],
        });
      }
      if (path === "/api/v1/search") {
        return Response.json({
          requested_mode: "auto", actual_mode: "semantic",
          coverage: { binding_required: true, scoped_documents: 1, complete_documents: 1, state: "complete" },
          degradations: ["degraded_provenance"],
          results: [{ vault_uid: vaultID, node_id: 42, content_version_id: versionID, rank: 1, score: 0.8, path: "/Reports/report.pdf", evidence: [{ kind: "direct_file", vector_space_id: "b".repeat(64) }] }],
          truncated: false, trace: [{ code: "source_fence", count: 1 }],
        });
      }
      throw new Error(`unexpected request: ${path}`);
    });

    render(ProcessingDrawer, {
      session: "short-lived",
      node: { id: 42, name: "report.pdf", kind: "file", current_version_id: versionID, size: 2048, revision: 1, created_at: "", modified_at: "" },
      path: "/Reports/report.pdf", onclose: vi.fn(), onauthfailure: vi.fn(), onrendition: vi.fn(),
    });

    expect(await screen.findByText("Hosted provider")).toBeTruthy();
    expect(screen.getByText("Consent revoked")).toBeTruthy();
    expect(screen.getByText(/document data leaves this machine/i)).toBeTruthy();
    expect(await screen.findByText(/direct-file.*unavailable/i)).toBeTruthy();
    expect(screen.getByText(/^Required/)).toBeTruthy();
    expect(screen.getByText(/previous complete generation remains available/i)).toBeTruthy();

    await fireEvent.input(screen.getByRole("searchbox", { name: "Search this document version" }), { target: { value: "synthetic" } });
    await fireEvent.click(screen.getByRole("button", { name: "Search this version" }));
    expect(await screen.findByText(/direct-file result; no text excerpt/i)).toBeTruthy();
    expect(screen.getByText(/degraded_provenance/i)).toBeTruthy();
    expect(screen.getByText(/direct_file/i)).toBeTruthy();
  });

  it("surfaces consent expiry without claiming work started", async () => {
    const versionID = "11111111-1111-4111-8111-111111111111";
    const fingerprint = "a".repeat(64);
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const path = String(input);
      if (path === "/api/v1/processing/profiles") return Response.json([{ name: "private", fingerprint, rendition: true, embedding_bindings: [] }]);
      if (path === "/api/v1/processing/plans") return Response.json({ fingerprint, vault_uid: versionID, selector: { node_id: 42, content_version_id: versionID, profile: "private" }, profile_fingerprint: fingerprint, flow: [], disclosed_classes: [], retained_classes: [], estimate: { source_bytes: 1, provider_calls: 1, vector_spaces: 0 }, consent_required: true, consent_state: "expired", backup_consequence: "none" });
      if (path.startsWith("/api/v1/coverage?")) return Response.json({ vault_uid: versionID, profile_fingerprint: fingerprint, state: "missing", renditions: { name: "rendition", required: true, state: "missing", complete: 0, unavailable: 0, stale: 0, ineligible: 0, total: 1 }, embeddings: [] });
      if (path === "/api/v1/processing/jobs") return new Response(JSON.stringify({ status: 412, code: "processing_consent_expired", detail: "The reviewed consent expired before provider access." }), { status: 412, headers: { "Content-Type": "application/json" } });
      throw new Error(`unexpected request: ${path}`);
    });

    render(ProcessingDrawer, {
      session: "short-lived",
      node: { id: 42, name: "report.pdf", kind: "file", current_version_id: versionID, size: 1, revision: 1, created_at: "", modified_at: "" },
      path: "/Reports/report.pdf", onclose: vi.fn(), onauthfailure: vi.fn(), onrendition: vi.fn(),
    });

    await fireEvent.click(await screen.findByRole("button", { name: "Consent and run" }));
    expect(screen.getByText("Consent expired")).toBeTruthy();
    expect(await screen.findByText(/consent expired before provider access/i)).toBeTruthy();
    expect(screen.queryByText("DURABLE JOB")).toBeNull();
  });
});
