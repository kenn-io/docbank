import { afterEach, describe, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { cleanup, render, screen } from "@testing-library/svelte";
import RenditionDrawer from "./RenditionDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("sanitized Markdown drawer", () => {
  it("renders only the inert body while exposing immutable metadata", async () => {
    const attachmentID = "a".repeat(64);
    const buildID = "b".repeat(64);
    const body = "# Synthetic report\n\n- safe item\n";
    const artifact = `---\ndocbank:\n  contract: "docbank-sanitized-markdown/v1"\n  source:\n    sha256: "${"d".repeat(64)}"\n    format: "pdf"\n    media_type: "application/pdf"\n  rendition:\n    build_id: "${buildID}"\n    rendition_request_fingerprint: "${"e".repeat(64)}"\n    evidence_lexical_fingerprint: "${"f".repeat(64)}"\n    normalized_evidence_contract: "normalized-evidence/v1"\n    body_sha256: "${bytesToHex(sha256(utf8ToBytes(body)))}"\n    completeness: "complete"\n    truncated: false\n  document:\n    title: "Synthetic report"\n    language: "en"\n    unit_kind: "page"\n    unit_count: 1\n  navigation:\n    offset_base: "body"\n    complete: true\n    entries:\n      - key: "page:1"\n        kind: "page"\n        title: "Page 1"\n        line: 1\n        byte: 0\n---\n${body}`;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(artifact, {
      headers: {
        "Content-Type": "text/markdown; charset=utf-8",
        "X-Docbank-Rendition-Attachment": attachmentID,
        "X-Docbank-Rendition-Build": buildID,
        "X-Docbank-Rendition-Artifact": "c".repeat(64),
        "X-Docbank-Content-Version": "11111111-1111-4111-8111-111111111111",
        "X-Docbank-Rendition-Profile": "e".repeat(64),
        "X-Docbank-Rendition-Completeness": "complete",
        "X-Docbank-Rendition-Warnings": "degraded_provenance",
        "X-Docbank-Blob-Hash": bytesToHex(sha256(utf8ToBytes(artifact))),
        "X-Docbank-Blob-Size": String(utf8ToBytes(artifact).length),
      },
    }));

    const { container } = render(RenditionDrawer, {
      session: "short-lived",
      attachmentID,
      path: "/Reports/report.pdf",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText(/Synthetic report/)).toBeTruthy();
    expect(screen.getAllByText("Complete").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(buildID)).toBeTruthy();
    expect(screen.getByText("Page 1")).toBeTruthy();
    expect(screen.getByText(/body-relative byte 0/i)).toBeTruthy();
    expect(screen.getByText("degraded_provenance")).toBeTruthy();
    const prose = container.querySelector(".rendition-body");
    expect(prose?.textContent).toContain("safe item");
    expect(prose?.textContent).not.toContain("body_sha256");
  });

  it("shows the no-readable-mirror state without rendering unverified prose", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(
      JSON.stringify({ status: 503, code: "rendition_unavailable", detail: "No verified rendition mirror is readable." }),
      { status: 503, headers: { "Content-Type": "application/json" } },
    ));
    const { container } = render(RenditionDrawer, {
      session: "short-lived", attachmentID: "a".repeat(64), path: "/Reports/report.pdf",
      onclose: vi.fn(), onauthfailure: vi.fn(),
    });

    expect(await screen.findByText(/no verified rendition mirror is readable/i)).toBeTruthy();
    expect(screen.getByText(/no readable verified mirror/i)).toBeTruthy();
    expect(container.querySelector(".rendition-body")).toBeNull();
  });
});
