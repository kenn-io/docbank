import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import VerifiedText from "./VerifiedText.svelte";
import type { SelectedSource } from "./selectedSource.js";

const source: SelectedSource = {
  kind: "live",
  key: "selected-image",
  nodeID: 7,
  mutationRevision: 3,
  versionID: "11111111-1111-4111-8111-111111111111",
  blobHash: "a".repeat(64),
  size: 50,
  name: "scan.png",
  path: "/scan.png",
  mimeType: "image/png",
  modifiedAt: "2026-09-11T12:00:00Z",
};
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it.each(["unconfigured", "unprocessed", "failed", "verified_empty"] as const)(
  "offers the %s text state without an empty Find toolbar",
  async (state) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json(
        textReceipt(source, {
          state,
          profile: {
            name: "",
            configuration: state === "unconfigured" ? "unconfigured" : "configured",
            fingerprint: state === "unconfigured" ? "" : "b".repeat(64),
          },
        }),
      ),
    );
    const onprocess = vi.fn();
    render(VerifiedText, {
      session: "session",
      source,
      authorizationRevision: 3,
      onauthfailure: vi.fn(),
      onprocess,
    });
    if (state === "unconfigured") {
      expect(await screen.findByText("Text extraction isn't set up.")).toBeTruthy();
      expect(screen.getByRole("link", { name: "Set up" }).getAttribute("href")).toBe(
        "https://docbank.ai/docs/usage/document-processing/",
      );
    } else if (state === "unprocessed") {
      await fireEvent.click(await screen.findByRole("button", { name: "Process…" }));
      expect(onprocess).toHaveBeenCalledOnce();
    } else if (state === "failed") {
      expect(
        await screen.findByText("Text processing failed for the selected profile."),
      ).toBeTruthy();
    } else {
      expect(
        await screen.findByText("Processing verified that this document has no readable text."),
      ).toBeTruthy();
      expect(screen.getByLabelText("Verified")).toBeTruthy();
    }
    expect(screen.queryByRole("searchbox")).toBeNull();
  },
);

function textReceipt(source: SelectedSource, overrides: object) {
  return {
    source: {
      node_id: 7,
      revision: 3,
      version_id: source.versionID,
      blob_hash: source.blobHash,
      size: source.size,
      media_type: source.mimeType,
    },
    profile: { name: "archive", configuration: "configured", fingerprint: "b".repeat(64) },
    generation_id: "",
    attachment_id: "",
    build_id: "",
    ...overrides,
  };
}

async function mockReadyText(source: SelectedSource, pages: number[] = [], artifactStatus = 200) {
  const { createHash } = await import("node:crypto");
  const multiplePages = pages.length > 1;
  const body = multiplePages
    ? "Report one. Report two.\nPage two.\nPage five.\nPage six."
    : "Report one. Report two.";
  const envelope = {
    docbank: {
      contract: "docbank-sanitized-markdown/v1",
      source: {
        sha256: source.blobHash,
        format: source.mimeType === "application/pdf" ? "pdf" : "png",
        media_type: source.mimeType,
      },
      rendition: {
        build_id: "e".repeat(64),
        rendition_request_fingerprint: "a".repeat(64),
        evidence_lexical_fingerprint: "b".repeat(64),
        normalized_evidence_contract: "normalized-evidence/v1",
        body_sha256: createHash("sha256").update(body).digest("hex"),
        completeness: pages.length ? "partial" : "complete",
        truncated: false,
      },
      document: { unit_kind: "page", unit_count: multiplePages ? 4 : 1 },
      navigation: {
        offset_base: "body",
        complete: true,
        entries: [
          { key: "page:1", kind: "page", line: 1, byte: 0 },
          ...(multiplePages
            ? [
                { key: "page:2", kind: "page", line: 2, byte: 24 },
                { key: "page:5", kind: "page", line: 3, byte: 34 },
                { key: "page:6", kind: "page", line: 4, byte: 45 },
              ]
            : []),
        ],
      },
    },
  };
  const bytes = new TextEncoder().encode(`---\n${JSON.stringify(envelope)}\n---\n${body}`);
  const hash = createHash("sha256").update(bytes).digest("hex");
  vi.spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(
      Response.json(
        textReceipt(source, {
          state: "ready",
          ...(pages.length ? { pages_without_text: pages } : {}),
          generation_id: "c".repeat(64),
          attachment_id: "d".repeat(64),
          build_id: "e".repeat(64),
          artifact: {
            id: "f".repeat(64),
            sha256: hash,
            size: bytes.length,
            media_type: "text/markdown; charset=utf-8",
          },
        }),
      ),
    )
    .mockResolvedValueOnce(
      new Response(bytes, {
        status: artifactStatus,
        headers: {
          "Content-Type": "text/markdown; charset=utf-8",
          "Content-Length": String(bytes.length),
          "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
          "X-Docbank-Rendition-SHA256": hash,
          "X-Docbank-Rendition-Size": String(bytes.length),
          "X-Docbank-Rendition-Profile": "b".repeat(64),
          "X-Docbank-Rendition-Generation": "c".repeat(64),
          "X-Docbank-Rendition-Attachment": "d".repeat(64),
          "X-Docbank-Rendition-Build": "e".repeat(64),
          "X-Docbank-Rendition-Artifact": "f".repeat(64),
        },
      }),
    );
}

const pdf = { ...source, name: "scan.pdf", path: "/scan.pdf", mimeType: "application/pdf" };

function renderPDF(receipt?: object) {
  if (receipt)
    vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(textReceipt(pdf, receipt)));
  render(VerifiedText, {
    session: "session",
    source: pdf,
    authorizationRevision: 3,
    profileName: "archive",
    onauthfailure: vi.fn(),
  });
}

it("finds text, wraps navigation, and clears the find query", async () => {
  await mockReadyText(source);
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  render(VerifiedText, {
    session: "synthetic",
    source,
    authorizationRevision: 3,
    onauthfailure: vi.fn(),
  });
  const input = await screen.findByLabelText("Find in verified text");
  await fireEvent.input(input, { target: { value: "report" } });
  expect(screen.getByText("1 of 2")).toBeTruthy();
  expect(document.querySelectorAll("mark")).toHaveLength(2);
  await fireEvent.keyDown(input, { key: "Enter" });
  expect(screen.getByText("2 of 2")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Next text match" }));
  expect(screen.getByText("1 of 2")).toBeTruthy();
  await fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
  expect(screen.getByText("2 of 2")).toBeTruthy();
  await fireEvent.input(input, { target: { value: "absent" } });
  expect(screen.getByText("No matches")).toBeTruthy();
  await fireEvent.input(input, { target: { value: "" } });
  expect(document.querySelectorAll("mark")).toHaveLength(0);
  Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
});

it("shows PDF page gaps before ready text", async () => {
  await mockReadyText(pdf, [3, 4, 7]);
  renderPDF();
  const rendered = await screen.findByText(/Pages 3–4, 7 have no text layer/);
  expect(screen.getByRole("link", { name: /OCR can read them/ }).getAttribute("href")).toBe(
    "https://docbank.ai/docs/usage/document-processing/#ocr",
  );
  expect(
    rendered.compareDocumentPosition(screen.getByLabelText("Verified text of scan.pdf")) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
});

it("shows only the page gap notice for a verified-empty PDF", async () => {
  renderPDF({
    state: "verified_empty",
    pages_without_text: [1],
    generation_id: "c".repeat(64),
    attachment_id: "d".repeat(64),
    build_id: "e".repeat(64),
  });
  await screen.findByText(/Page 1 has no text layer/);
  expect(screen.getAllByRole("status")).toHaveLength(1);
});

it("retains PDF page gaps when reading the text artifact fails", async () => {
  await mockReadyText(pdf, [2], 500);
  renderPDF();
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.getByText(/Page 2 has no text layer/)).toBeTruthy();
});
it("offers processing instead of setup for an unconfigured PDF", async () => {
  renderPDF({ state: "unconfigured", pages_without_text: [2] });
  expect(await screen.findByText(/Page 2 has no text layer/)).toBeTruthy();
  expect(screen.getByText("This version hasn't been processed.")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Set up" })).toBeNull();
});

it("shows unavailable PDF page coverage beside the selected profile state", async () => {
  renderPDF({ state: "failed", page_coverage: "unavailable" });
  expect(await screen.findByText("PDF page text coverage is unavailable.")).toBeTruthy();
  expect(screen.getByText("Text processing failed for the selected profile.")).toBeTruthy();
});
