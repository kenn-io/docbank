import bridgeSource from "./email-frame-bridge.js?raw";
import { pageDigest } from "./pages.js";
import { emailTupleDigest } from "./email-viewer.js";
import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import userEvent from "@testing-library/user-event";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import VerifiedPreview from "./VerifiedPreview.svelte";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

function source(content: Uint8Array, versionID: string, mimeType = "text/plain"): SelectedSource {
  const blobHash = createHash("sha256").update(content).digest("hex");
  return {
    kind: "snapshot",
    key: `snapshot:7:${versionID}:${blobHash}:${content.length}`,
    nodeID: 7,
    mutationRevision: 3,
    versionID,
    blobHash,
    size: content.length,
    name: mimeType.startsWith("image/") ? "scan.png" : "report.txt",
    path: mimeType.startsWith("image/") ? "/scan.png" : "/report.txt",
    mimeType,
    modifiedAt: "2026-09-10T12:00:00Z",
    observedAt: "2026-09-11T12:00:00Z",
    originalTags: [],
    collectionIDs: [],
  };
}

function ready(selected: SelectedSource, ticket: string): Response {
  return new Response(
    `${JSON.stringify({
      phase: "ready",
      received: selected.size,
      total: selected.size,
      url: `/api/daemon/web-download/file?ticket=${ticket}`,
      name: selected.name,
      version_id: selected.versionID,
      blob_hash: selected.blobHash,
    })}\n`,
  );
}

function body(selected: SelectedSource, bytes: Uint8Array): Response {
  return new Response(Uint8Array.from(bytes).buffer, {
    headers: {
      "Content-Type": selected.mimeType,
      "Content-Length": String(bytes.length),
      "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
      "X-Docbank-Content-Version": selected.versionID,
      "X-Docbank-Blob-Hash": selected.blobHash,
      "X-Docbank-Blob-Size": String(selected.size),
    },
  });
}

it("keeps Email available and selected for a parameterized email MIME type", async () => {
  const selected = source(
    new TextEncoder().encode("Subject: Synthetic\r\n\r\nBody"),
    "11111111-1111-4111-8111-111111111111",
    "MESSAGE/RFC822\t; charset=utf-8",
  );
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ state: "pending" }, { status: 202 }),
  );
  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    activeTab: "email",
    onauthfailure: vi.fn(),
  });
  expect(screen.getByRole("tab", { name: "Email" }).getAttribute("aria-selected")).toBe("true");
  expect(await screen.findByText(/Email decoding is pending/)).toBeTruthy();
});

it("falls back to verified Preview when an Email tab survives non-email selection", async () => {
  const bytes = new TextEncoder().encode("Selected ordinary document");
  const selected = source(bytes, "11111111-1111-4111-8111-111111111111");
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (init?.method === "POST") return ready(selected, "ordinary");
    if (String(input).endsWith("ticket=ordinary")) return body(selected, bytes);
    throw new Error(`Unexpected request ${String(input)}`);
  });
  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    activeTab: "email",
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Selected ordinary document")).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true");
  expect(screen.queryByRole("tab", { name: "Email" })).toBeNull();
});

it("shows only the newly selected exact version when source and session change", async () => {
  const oldBytes = new TextEncoder().encode("old frozen text");
  const newBytes = new TextEncoder().encode("new selected text");
  const oldSource = source(oldBytes, "11111111-1111-4111-8111-111111111111");
  const newSource = source(newBytes, "22222222-2222-4222-8222-222222222222");
  let oldBodySignal: AbortSignal | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (init?.method === "POST") {
      const request = JSON.parse(String(init.body)) as { version_id: string };
      return request.version_id === oldSource.versionID
        ? ready(oldSource, "old")
        : ready(newSource, "new");
    }
    if (path.endsWith("ticket=old") && init?.method !== "DELETE") {
      oldBodySignal = init?.signal ?? undefined;
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("cancelled", "AbortError")),
        );
      });
    }
    if (path.endsWith("ticket=new")) return body(newSource, newBytes);
    if (init?.method === "DELETE") return new Response(null, { status: 204 });
    throw new Error(`unexpected request ${path}`);
  });
  const view = render(VerifiedPreview, {
    session: "first-session",
    source: oldSource,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });
  await waitFor(() => expect(oldBodySignal).toBeTruthy());

  await view.rerender({
    session: "second-session",
    source: newSource,
    authorizationRevision: 9,
    onauthfailure: vi.fn(),
  });

  expect(await screen.findByText("new selected text")).toBeTruthy();
  expect(oldBodySignal?.aborted).toBe(true);
  expect(screen.queryByText("old frozen text")).toBeNull();
});

it.each([
  ["image/jpeg", false],
  ["image/png", false],
  ["image/png", true],
] as const)(
  "previews %s without page frames (runtime %s) and revokes its URL",
  async (mimeType, runtime) => {
    const bytes = new Uint8Array([137, 80, 78, 71]);
    const selected = source(bytes, "33333333-3333-4333-8333-333333333333", mimeType);
    const fetcher = vi.spyOn(globalThis, "fetch");
    if (mimeType === "image/png")
      fetcher.mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            runtime_available: runtime,
            inventory: {
              source: {
                version_id: selected.versionID,
                sha256: selected.blobHash,
                size: selected.size,
              },
              page_count: 0,
              frames: [],
              recipes: [],
              images: [],
            },
          }),
        ),
      );
    fetcher
      .mockResolvedValueOnce(ready(selected, "image"))
      .mockResolvedValueOnce(body(selected, bytes));
    const createObjectURL = vi.fn(() => "blob:verified-image");
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, "createObjectURL", {
      configurable: true,
      value: createObjectURL,
    });
    Object.defineProperty(URL, "revokeObjectURL", {
      configurable: true,
      value: revokeObjectURL,
    });

    const view = render(VerifiedPreview, {
      session: "session",
      source: selected,
      authorizationRevision: 8,
      onauthfailure: vi.fn(),
    });
    expect(
      (await screen.findByRole("img", { name: "Verified preview of scan.png" })).getAttribute(
        "src",
      ),
    ).toBe("blob:verified-image");
    expect(createObjectURL).toHaveBeenCalledOnce();

    view.unmount();
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:verified-image");
  },
);

it("keeps unsupported content on the download fallback", async () => {
  const selected = source(
    new TextEncoder().encode("<svg/>"),
    "44444444-4444-4444-8444-444444444444",
    "image/svg+xml",
  );
  const fetchMock = vi.spyOn(globalThis, "fetch");

  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });

  expect(await screen.findByText(/not an eligible text or raster image preview/i)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Download verified original" })).toBeTruthy();
  expect(fetchMock).not.toHaveBeenCalled();
});

it("loads one exact rendition only while Text is active and keeps hostile markup inert", async () => {
  const original = new TextEncoder().encode("%PDF synthetic");
  const selected = source(original, "55555555-5555-4555-8555-555555555555", "application/pdf");
  const markdown = "Alpha 😀 <script>alert('blocked')</script> alpha";
  const envelope = {
    docbank: {
      contract: "docbank-sanitized-markdown/v1",
      source: { sha256: selected.blobHash, format: "pdf", media_type: selected.mimeType },
      rendition: {
        build_id: "e".repeat(64),
        rendition_request_fingerprint: "a".repeat(64),
        evidence_lexical_fingerprint: "b".repeat(64),
        normalized_evidence_contract: "normalized-evidence/v1",
        body_sha256: createHash("sha256").update(markdown).digest("hex"),
        completeness: "complete",
        truncated: false,
      },
      document: { unit_kind: "page", unit_count: 1 },
      navigation: {
        offset_base: "body",
        complete: true,
        entries: [{ key: "page:1", kind: "page", line: 1, byte: 0 }],
      },
    },
  };
  const text = new TextEncoder().encode(`---\n${JSON.stringify(envelope)}\n---\n${markdown}`);
  const digest = createHash("sha256").update(text).digest("hex");
  const authority = {
    profile: "b".repeat(64),
    generation: "c".repeat(64),
    attachment: "d".repeat(64),
    build: "e".repeat(64),
    artifact: `artifact_${"f".repeat(64)}`,
  };
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const path = String(input);
    if (path.endsWith("/api/v1/pages/inventory"))
      return new Response(
        JSON.stringify({
          runtime_available: false,
          inventory: {
            source: {
              version_id: selected.versionID,
              sha256: selected.blobHash,
              size: selected.size,
            },
            page_count: 0,
            frames: [],
            recipes: [],
            images: [],
          },
        }),
      );
    if (path.endsWith("/api/v1/renditions/text"))
      return new Response(
        JSON.stringify({
          state: "ready",
          source: {
            node_id: selected.nodeID,
            revision: 8,
            version_id: selected.versionID,
            blob_hash: selected.blobHash,
            size: selected.size,
            media_type: selected.mimeType,
          },
          profile: { name: "archive", configuration: "configured", fingerprint: authority.profile },
          generation_id: authority.generation,
          attachment_id: authority.attachment,
          build_id: authority.build,
          artifact: {
            id: authority.artifact,
            sha256: digest,
            size: text.length,
            media_type: "text/markdown; charset=utf-8",
          },
        }),
        { headers: { "Content-Type": "application/json" } },
      );
    if (path.includes("/api/v1/renditions/text/content?"))
      return new Response(text, {
        headers: {
          "Content-Type": "text/markdown; charset=utf-8",
          "Content-Length": String(text.length),
          "Content-Digest": `sha-256=:${createHash("sha256").update(text).digest("base64")}:`,
          "X-Docbank-Rendition-SHA256": digest,
          "X-Docbank-Rendition-Size": String(text.length),
          "X-Docbank-Rendition-Profile": authority.profile,
          "X-Docbank-Rendition-Generation": authority.generation,
          "X-Docbank-Rendition-Attachment": authority.attachment,
          "X-Docbank-Rendition-Build": authority.build,
          "X-Docbank-Rendition-Artifact": authority.artifact,
        },
      });
    throw new Error(`unexpected request ${path}`);
  });
  const props = {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    profileName: "archive",
    observed: {
      configuration: "configured" as const,
      profileFingerprint: authority.profile,
      generationID: authority.generation,
      coverageState: "complete" as const,
      attachmentID: authority.attachment,
      buildID: authority.build,
    },
    queryTerms: ["alpha"],
    onauthfailure: vi.fn(),
  };
  const view = render(VerifiedPreview, props);
  await screen.findByText(/No page inventory/);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  expect(await screen.findByText(/<script>alert\('blocked'\)<\/script>/)).toBeTruthy();
  expect(document.querySelector("script")).toBeNull();
  expect(document.querySelectorAll("mark[data-source='query']")).toHaveLength(2);
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/")),
  ).toHaveLength(3);

  await view.rerender({ ...props, queryTerms: ["alpha", "blocked"] });
  await waitFor(() => expect(document.querySelectorAll("mark")).toHaveLength(3));
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/")),
  ).toHaveLength(3);
});

it.each([
  ["failed", "Text processing failed for this selected version."],
  ["unprocessed", "This selected version has not been processed for text."],
  ["unconfigured", "Text processing is not configured for this vault."],
])("keeps the %s reason visible with verified original-text fallback", async (state, message) => {
  const bytes = new TextEncoder().encode("Synthetic fallback evidence");
  const selected = source(bytes, "66666666-6666-4666-8666-666666666666");
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (path === "/api/v1/renditions/text")
      return new Response(
        JSON.stringify({
          state,
          source: {
            node_id: selected.nodeID,
            revision: 8,
            version_id: selected.versionID,
            blob_hash: selected.blobHash,
            size: selected.size,
            media_type: selected.mimeType,
          },
          profile: {
            name: "",
            configuration: state === "unconfigured" ? "unconfigured" : "configured",
            fingerprint: state === "unconfigured" ? "" : "b".repeat(64),
          },
          generation_id: "",
          attachment_id: "",
          build_id: "",
        }),
        { headers: { "Content-Type": "application/json" } },
      );
    if (init?.method === "POST") return ready(selected, "fallback");
    if (path.endsWith("ticket=fallback")) return body(selected, bytes);
    throw new Error(`unexpected request ${path}`);
  });
  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    activeTab: "text",
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Synthetic fallback evidence")).toBeTruthy();
  expect(screen.getByText(message).getAttribute("role")).toBe("status");
});

it.each([0, 64 * 1024 * 1024 + 1])(
  "offers download without requesting pages for a %i-byte PDF",
  async (size) => {
    const selected = {
      ...source(new Uint8Array([1]), "33333333-3333-4333-8333-333333333333", "application/pdf"),
      size,
    };
    const fetcher = vi.spyOn(globalThis, "fetch");
    render(VerifiedPreview, {
      session: "session",
      source: selected,
      authorizationRevision: 8,
      onauthfailure: vi.fn(),
    });
    await screen.findByText(/Download the verified original/);
    expect(screen.queryByRole("button", { name: /Refresh|Render/ })).toBeNull();
    expect(fetcher).not.toHaveBeenCalled();
  },
);

it("reports inspection gestures across UTC days and ignores unrelated keys", async () => {
  const bytes = new TextEncoder().encode("Synthetic inspected text");
  const selected = source(bytes, "55555555-5555-4555-8555-555555555555");
  const events: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === "/api/daemon/telemetry/events") {
      events.push(JSON.parse(String(init?.body)));
      return new Response(null, { status: 202 });
    }
    if (init?.method === "POST") return ready(selected, "synthetic");
    if (String(input).endsWith("ticket=synthetic")) return body(selected, bytes);
    throw new Error(`Unexpected request ${String(input)}`);
  });
  render(VerifiedPreview, {
    session: "visit-session",
    source: selected,
    authorizationRevision: 3,
    onauthfailure: vi.fn(),
  });
  await screen.findByText("Synthetic inspected text");
  expect(events).toEqual([]);
  vi.setSystemTime(new Date("2026-10-17T23:59:00Z"));
  await fireEvent.keyDown(screen.getByText("Synthetic inspected text"), { key: "Tab" });
  await fireEvent.keyDown(screen.getByText("Synthetic inspected text"), { key: "a" });
  await fireEvent.keyDown(screen.getByText("Synthetic inspected text"), {
    key: "j",
    ctrlKey: true,
  });
  expect(events).toEqual([]);
  await fireEvent.wheel(screen.getByText("Synthetic inspected text"), { deltaY: 120 });
  const opened = { event: "document_opened", properties: { surface: "web" } };
  await waitFor(() => expect(events).toEqual([opened]));
  await fireEvent.wheel(screen.getByText("Synthetic inspected text"), { deltaY: 120 });
  expect(events).toEqual([opened]);
  vi.setSystemTime(new Date("2026-10-18T00:01:00Z"));
  await fireEvent.keyDown(screen.getByText("Synthetic inspected text"), { key: "ArrowDown" });
  await waitFor(() => expect(events).toEqual([opened, opened]));
  await fireEvent.pointerDown(screen.getByText("Synthetic inspected text"));
  expect(events).toEqual([opened, opened]);
});

it.each(["Text", "Email"])(
  "reports keyboard activation of %s for an automatically selected document",
  async (tabName) => {
    const selected = source(
      new TextEncoder().encode("Subject: Synthetic\r\n\r\nBody"),
      "11111111-1111-4111-8111-111111111111",
      "message/rfc822",
    );
    const events: unknown[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (String(input) === "/api/daemon/telemetry/events") {
        events.push(JSON.parse(String(init?.body)));
        return new Response(null, { status: 202 });
      }
      if (String(input) === "/api/v1/renditions/text")
        return Response.json({
          state: "unconfigured",
          source: {
            node_id: selected.nodeID,
            revision: 8,
            version_id: selected.versionID,
            blob_hash: selected.blobHash,
            size: selected.size,
            media_type: selected.mimeType,
          },
          profile: { name: "", configuration: "unconfigured", fingerprint: "" },
          generation_id: "",
          attachment_id: "",
          build_id: "",
        });
      return Response.json({ state: "pending" }, { status: 202 });
    });
    render(VerifiedPreview, {
      session: "session",
      source: selected,
      authorizationRevision: 8,
      onauthfailure: vi.fn(),
    });
    const tab = screen.getByRole("tab", { name: tabName });
    const user = userEvent.setup();
    for (let attempt = 0; attempt < 10 && document.activeElement !== tab; attempt++)
      await user.tab();
    expect(document.activeElement).toBe(tab);
    expect(events).toEqual([]);
    await user.keyboard("{Enter}");
    expect(tab.getAttribute("aria-selected")).toBe("true");
    await waitFor(() =>
      expect(events).toEqual([{ event: "document_opened", properties: { surface: "web" } }]),
    );
  },
);

it("reports sandboxed email inspection gestures again after UTC rollover", async () => {
  const selected = source(
    new TextEncoder().encode("Synthetic email"),
    "55555555-5555-4555-8555-555555555555",
    "message/rfc822",
  );
  const html = new TextEncoder().encode("<p>Synthetic email body</p>");
  const artifact = {
    role: "body_utf8",
    sha256: createHash("sha256").update(html).digest("hex"),
    size: html.length,
  };
  const evidence = {
    contract_version: "docbank-email/v1",
    source: { sha256: selected.blobHash, size: selected.size, verification: "verified" },
    recipe: {},
    outcome: "decoded",
    failure: null,
    inventory: {
      state: "complete",
      root_path: "1",
      diagnostics: [],
      parts: [
        {
          path: "1",
          parent_path: null,
          message_path: "1",
          media: { declared: "text/html", detected: null },
          content_id: { value: null, state: "absent" },
          decode_state: "decoded",
          filename: { safe_name: "" },
          disposition: null,
          payload: null,
          body_utf8: artifact,
          header_block: null,
          diagnostics: [],
        },
      ],
      messages: [
        {
          path: "1",
          selected_body_path: "1",
          alternatives: [
            { part_path: "1", kind: "html", display_state: "available", display: artifact },
          ],
          fields: {},
          date: { state: "absent", civil: null, timezone_text: null, timezone_state: "absent" },
          diagnostics: [],
        },
      ],
    },
  };
  const checksum = pageDigest(evidence);
  const recipe = pageDigest(evidence.recipe);
  const generation = emailTupleDigest(
    "docbank-email-generation/v1",
    evidence.contract_version,
    selected.blobHash,
    String(selected.size),
    recipe,
    checksum,
  );
  const attachment = emailTupleDigest(
    "docbank-email-attachment/v1",
    selected.versionID,
    generation,
  );
  const events: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/daemon/telemetry/events") {
      events.push(JSON.parse(String(init?.body)));
      return new Response(null, { status: 202 });
    }
    if (url.endsWith("/email"))
      return new Response(
        JSON.stringify({
          version: {
            id: selected.versionID,
            node_id: selected.nodeID,
            blob_hash: selected.blobHash,
            size: selected.size,
          },
          evidence,
          checksum,
          recipe_fingerprint: recipe,
          generation_id: generation,
          attachment_id: attachment,
        }),
        { headers: { "Content-Type": "application/json" } },
      );
    if (url === `/api/v1/nodes/${selected.nodeID}`)
      return new Response(
        JSON.stringify({ id: selected.nodeID, kind: "file", name: "synthetic.eml" }),
        { headers: { "Content-Type": "application/json" } },
      );
    if (url.includes("/email/"))
      return new Response(html, {
        headers: {
          "Content-Type": "application/octet-stream",
          "X-Docbank-Content-Version": selected.versionID,
          "X-Docbank-Email-Generation": generation,
          "X-Docbank-Email-Attachment": attachment,
          "X-Docbank-Email-Part-Path": "1",
          "X-Docbank-Email-Part-Role": "body_utf8",
          "X-Docbank-Blob-Hash": artifact.sha256,
          "X-Docbank-Blob-Size": String(html.length),
        },
      });
    throw new Error(`Unexpected request ${url}`);
  });
  render(VerifiedPreview, {
    session: "visit-session",
    source: selected,
    authorizationRevision: 3,
    activeTab: "email",
    onauthfailure: vi.fn(),
  });
  const frame = (await screen.findByTitle(
    `Email HTML body of ${selected.name}`,
  )) as HTMLIFrameElement;
  const frameDocument = frame.contentDocument!;
  frameDocument.open();
  frameDocument.write(frame.srcdoc);
  frameDocument.close();
  // jsdom does not load the sandbox's external script. Run that exact bridge
  // against its document and forward the browser's opaque-origin envelope.
  new Function("document", "parent", bridgeSource)(frameDocument, {
    postMessage: (data: unknown) =>
      window.dispatchEvent(
        new MessageEvent("message", { data, source: frame.contentWindow, origin: "null" }),
      ),
  });
  await fireEvent.keyDown(frameDocument.body, { key: "Escape" });
  expect(document.activeElement).toBe(
    screen.getByRole("button", { name: "Enter email body · Escape returns here" }),
  );
  expect(events).toEqual([]);
  const opened = { event: "document_opened", properties: { surface: "web" } };
  vi.setSystemTime(new Date("2026-10-17T23:59:00Z"));
  await fireEvent.pointerDown(frameDocument.body);
  expect(events).toEqual([opened]);
  vi.setSystemTime(new Date("2026-10-18T00:01:00Z"));
  await fireEvent.wheel(frameDocument.body);
  expect(events).toEqual([opened, opened]);
  vi.setSystemTime(new Date("2026-10-19T00:01:00Z"));
  await fireEvent.keyDown(frameDocument.body, { key: "j", ctrlKey: true });
  expect(events).toEqual([opened, opened]);
  await fireEvent.keyDown(frameDocument.body, { key: "j" });
  expect(events).toEqual([opened, opened, opened]);
});
