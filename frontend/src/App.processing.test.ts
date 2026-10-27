import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { hop } from "./processing-test-fixtures.js";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function setup(entry: "header" | "text", deferCompletion = false) {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const versionID = "11111111-1111-4111-8111-111111111111";
  const fingerprint = "a".repeat(64);
  const root = {
    id: 1,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    created_at: "2026-08-28T00:00:00Z",
    modified_at: "2026-08-28T00:00:00Z",
    path: "/",
  };
  const file = {
    id: 2,
    parent_id: 1,
    name: "report.pdf",
    kind: "file",
    current_version_id: versionID,
    blob_hash: "b".repeat(64),
    size: 2048,
    mime_type: "application/pdf",
    revision: 1,
    created_at: "2026-08-28T00:00:00Z",
    modified_at: "2026-08-28T00:00:00Z",
  };
  const json = (value: unknown) => Response.json(value);
  let extracted = false;
  let finishProcessing!: () => void;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/2") return json({ ...file, path: "/report.pdf" });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/audit/status?node_id=2") return json({ enabled: false, scopes: [] });
    if (url === "/api/v1/nodes/2/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/renditions/text")
      return json({
        state: extracted ? "verified_empty" : "unprocessed",
        source: {
          node_id: 2,
          revision: 1,
          version_id: versionID,
          blob_hash: file.blob_hash,
          size: file.size,
          media_type: file.mime_type,
        },
        profile: { name: "", configuration: "configured", fingerprint },
        generation_id: "",
        attachment_id: "",
        build_id: "",
      });
    if (url === "/api/v1/processing/jobs") {
      const jobID = "c".repeat(64);
      const encoder = new TextEncoder();
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(
              encoder.encode(
                `${JSON.stringify({
                  sequence: 1,
                  type: "job",
                  job: {
                    id: jobID,
                    attachment_id: "d".repeat(64),
                    embedding_job_ids: [],
                    profile_fingerprint: fingerprint,
                    content_version_id: versionID,
                  },
                })}\n`,
              ),
            );
            finishProcessing = () => {
              extracted = true;
              controller.enqueue(
                encoder.encode(
                  `${JSON.stringify({
                    sequence: 2,
                    type: "status",
                    status: {
                      job_id: jobID,
                      state: "completed",
                      phase: "published",
                      embedding_job_ids: [],
                      completed_bindings: 0,
                    },
                    terminal: true,
                  })}\n`,
                ),
              );
              controller.close();
            };
            if (!deferCompletion) finishProcessing();
          },
        }),
        { headers: { "Content-Type": "application/x-ndjson" } },
      );
    }
    if (url === "/api/v1/processing/profiles")
      return json([
        {
          name: "private",
          fingerprint,
          rendition: true,
          embedding_bindings: entry === "header" ? ["embed"] : [],
        },
      ]);
    if (url === "/api/v1/processing/plans")
      return json({
        fingerprint,
        vault_uid: versionID,
        selector: { node_id: 2, content_version_id: versionID, profile: "private" },
        profile_fingerprint: fingerprint,
        flow: [hop()],
        disclosed_classes: ["original_file"],
        retained_classes: ["sanitized_markdown"],
        estimate: { source_bytes: 2048, provider_calls: 1, vector_spaces: 0 },
        consent_required: false,
        consent_state: "active",
        backup_consequence: "retained derivatives enter future backups",
      });
    if (url.startsWith("/api/v1/coverage?"))
      return json({
        vault_uid: versionID,
        profile_fingerprint: fingerprint,
        state: "missing",
        renditions: {
          name: "rendition",
          required: true,
          state: "missing",
          complete: 0,
          unavailable: 0,
          stale: 0,
          ineligible: 0,
          total: 1,
        },
        embeddings: [],
      });
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  return { fetchMock, versionID, finishProcessing: () => finishProcessing() };
}

it.each(["header", "text"] as const)("opens exact-version processing from %s", async (entry) => {
  const { fetchMock, versionID } = setup(entry);
  await fireEvent.click(await screen.findByRole("cell", { name: "report.pdf" }));
  if (entry === "text") {
    await fireEvent.click(await screen.findByRole("tab", { name: "Text" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Process…" }));
  } else {
    await fireEvent.click(await screen.findByRole("button", { name: "Extract text" }));
  }
  const dialog = await screen.findByRole("dialog", { name: "Extract text" });
  await waitFor(() =>
    expect(fetchMock.mock.calls.some(([input]) => input === "/api/v1/processing/plans")).toBe(true),
  );
  const planned = fetchMock.mock.calls.find(([input]) => input === "/api/v1/processing/plans");
  expect(JSON.parse(String(planned?.[1]?.body)).selector).toEqual({
    node_id: 2,
    content_version_id: versionID,
    profile: "private",
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
  const lens = "Find documents similar to report.pdf";
  if (entry === "text") {
    // Without a profile that stores search vectors the lens would lead nowhere.
    expect(screen.queryByRole("button", { name: lens })).toBeNull();
    return;
  }
  const similarButton = screen.getByRole("button", { name: lens });
  const enter = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
  similarButton.dispatchEvent(enter);
  expect(enter.defaultPrevented).toBe(false);
  await fireEvent.click(similarButton);
  expect(await screen.findByRole("region", { name: "Similar documents" })).toBeTruthy();
});

it.each([false, true])(
  "refreshes cached text after extraction, closes before completion=%s",
  async (closeBeforeCompletion) => {
    const { finishProcessing } = setup("text", closeBeforeCompletion);
    await fireEvent.click(await screen.findByRole("cell", { name: "report.pdf" }));
    await fireEvent.click(await screen.findByRole("tab", { name: "Text" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Process…" }));
    const dialog = await screen.findByRole("dialog", { name: "Extract text" });
    const run = await within(dialog).findByRole("button", { name: "Extract text" });
    await waitFor(() => expect(run.hasAttribute("disabled")).toBe(false));
    await fireEvent.click(run);
    if (closeBeforeCompletion) {
      await within(dialog).findByRole("button", { name: /Extracting…/ });
    } else {
      await within(dialog).findByText("Text extracted.");
    }
    await fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    if (closeBeforeCompletion) finishProcessing();
    expect(
      await screen.findByText("Processing verified that this document has no readable text."),
    ).toBeTruthy();
  },
);
