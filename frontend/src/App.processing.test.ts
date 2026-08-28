import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("opens processing for the selected exact document version", async () => {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const versionID = "11111111-1111-4111-8111-111111111111";
  const fingerprint = "a".repeat(64);
  const root = { id: 1, name: "", kind: "dir", size: 0, revision: 1, created_at: "2026-08-28T00:00:00Z", modified_at: "2026-08-28T00:00:00Z", path: "/" };
  const file = { id: 2, parent_id: 1, name: "report.pdf", kind: "file", current_version_id: versionID, blob_hash: "b".repeat(64), size: 2048, mime_type: "application/pdf", revision: 1, created_at: "2026-08-28T00:00:00Z", modified_at: "2026-08-28T00:00:00Z" };
  const json = (value: unknown) => Response.json(value);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/audit/status?node_id=2") return json({ enabled: false, scopes: [] });
    if (url === "/api/v1/nodes/2/tags?limit=1000&offset=0") return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/processing/profiles") return json([{ name: "private", fingerprint, rendition: true, embedding_bindings: [] }]);
    if (url === "/api/v1/processing/plans") return json({ fingerprint, vault_uid: versionID, selector: { node_id: 2, content_version_id: versionID, profile: "private" }, profile_fingerprint: fingerprint, flow: [], disclosed_classes: [], retained_classes: ["sanitized_markdown"], estimate: { source_bytes: 2048, provider_calls: 1, vector_spaces: 0 }, consent_required: false, consent_state: "active", backup_consequence: "retained derivatives enter future backups" });
    if (url.startsWith("/api/v1/coverage?")) return json({ vault_uid: versionID, profile_fingerprint: fingerprint, state: "missing", renditions: { name: "rendition", required: true, state: "missing", complete: 0, unavailable: 0, stale: 0, ineligible: 0, total: 1 }, embeddings: [] });
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "report.pdf" }));
  await fireEvent.click(screen.getByRole("button", { name: "Process and retrieve" }));
  expect(await screen.findByRole("dialog", { name: "Document processing and coverage" })).toBeTruthy();
  expect(screen.getByText(`Exact version ${versionID}`)).toBeTruthy();
  expect(await screen.findByText("Consent active")).toBeTruthy();
});
