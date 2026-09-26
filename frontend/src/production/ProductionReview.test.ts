import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import ProductionReview from "./ProductionReview.svelte";
import type { ProductionDraft, ProductionSet } from "./api.js";

// The review owns byte verification and focus; the PDF engine is covered by
// the real-daemon browser capture.
vi.mock("./SourcePDFViewer.svelte", () => ({ default: () => {} }));

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic review", creator: "test", created_at: "2026-09-25T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 4, state: "draft", membership_sealed: false };
const sourcePDF = new TextEncoder().encode("%PDF-1.7\nsynthetic review page\n%%EOF");
const sourcePDFSHA = bytesToHex(sha256(sourcePDF));
const member = (id: string, ordinal: number) => ({ id, ordinal, node_id: ordinal + 10, source_version_id: `00000000-0000-4000-8000-${String(ordinal).padStart(12, "0")}`, pdf_sha256: sourcePDFSHA, pdf_size: sourcePDF.length, map_sha256: "a".repeat(64), mode: "redact_selected", reviewed: false });
const memberA = member("22222222-2222-4222-8222-222222222222", 1);
const memberB = member("33333333-3333-4333-8333-333333333333", 2);
const flag = (id: string, memberID: string, page: number) => ({ id, member_id: memberID, action: "keep", uncertain: true,
  selector: { kind: "page", map_sha256: "a".repeat(64), pages: [page] } });
const flagA = flag("44444444-4444-4444-8444-444444444444", memberA.id, 1);
const flagB = flag("55555555-5555-4555-8555-555555555555", memberB.id, 2);
const mapRaw = new TextEncoder().encode(JSON.stringify({ contract: "aligned-text/v1", text: "Synthetic passage to review", pages: [] }));
const mapSHA = bytesToHex(sha256(mapRaw));
const textFlag = { ...flagA, selector: { kind: "text", map_sha256: mapSHA, span: { start: 0, end: 17 } },
  reason: "private synthetic rationale", label: "REDACTED", actor: "synthetic", created_at: "2026-09-26T00:00:00Z", revision: 2 };
function mapPage(): Response { return json({ map_sha256: mapSHA, offset: 0, total_bytes: mapRaw.length,
  data: Buffer.from(mapRaw).toString("base64"), chunk_sha256: bytesToHex(sha256(mapRaw)), next_cursor: "" }); }

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} }));
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

it("pages exact members and uncertain decisions without mixing revisions", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    requests.push(url);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "member-next" });
    if (url.endsWith("/members?limit=50&cursor=member-next")) return json({ items: [memberB], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [flagA], next_cursor: "flag-next" });
    if (url.endsWith("/decisions?limit=50&uncertain=true&cursor=flag-next")) return json({ items: [flagB], next_cursor: "" });
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  expect(await screen.findByText("Member 1")).toBeTruthy();
  expect(await screen.findByText("Page 1")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Load more members" }));
  await fireEvent.click(screen.getByRole("button", { name: "Load more flagged passages" }));
  expect(await screen.findByText("Member 2")).toBeTruthy();
  expect(await screen.findByText("Page 2")).toBeTruthy();
  expect(requests).toContain(`/api/v1/productions/sets/${set.id}/revisions/2/members?limit=50&cursor=member-next`);
  expect(requests).toContain(`/api/v1/productions/sets/${set.id}/revisions/2/decisions?limit=50&uncertain=true&cursor=flag-next`);
});

it("offers an exact original PDF for a retained member", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    throw new Error(`unexpected request ${url}`);
  });
  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  expect(await screen.findByRole("button", { name: "Open original PDF for member 1" })).toBeTruthy();
});

it("opens only verified source bytes and returns focus when closed", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    if (url.endsWith(`/members/${memberA.id}/pdf`)) return new Response(sourcePDF as BodyInit, { headers: {
      "Content-Type": "application/pdf", "X-Docbank-Blob-Hash": sourcePDFSHA,
      "X-Docbank-Blob-Size": String(sourcePDF.length),
      "Content-Digest": `sha-256=:${Buffer.from(sha256(sourcePDF)).toString("base64")}:`,
    } });
    throw new Error(`unexpected request ${url}`);
  });
  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  const open = await screen.findByRole("button", { name: "Open original PDF for member 1" });
  await fireEvent.click(open);
  expect(await screen.findByRole("region", { name: "Original PDF for member 1" }, { timeout: 10_000 })).toBeTruthy();
  const close = screen.getByRole("button", { name: "Close original PDF" });
  await waitFor(() => expect(document.activeElement).toBe(close), { timeout: 5_000 });
  await fireEvent.click(close);
  await waitFor(() => expect(document.activeElement).toBe(open), { timeout: 5_000 });
  expect(screen.queryByRole("region", { name: "Original PDF for member 1" })).toBeNull();
}, 20_000);

it("discards a page when its exact draft ETag changes during the read", async () => {
  const refresh = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json({ ...draft, etag: 5 });
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: refresh, onauthfailure: vi.fn(), onclose: vi.fn() });
  expect(await screen.findByText("Draft changed during review. Refresh to load the current version.")).toBeTruthy();
  expect(screen.queryByText("Member 1")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Refresh draft" }));
  expect(refresh).toHaveBeenCalledOnce();
});

it("retries the same member cursor after a lost page response", async () => {
  let secondPageReads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "member-next" });
    if (url.endsWith("/members?limit=50&cursor=member-next")) {
      secondPageReads++;
      if (secondPageReads === 1) throw new TypeError("synthetic lost page");
      return json({ items: [memberB], next_cursor: "" });
    }
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Load more members" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost page");
  await fireEvent.click(screen.getByRole("button", { name: "Retry members" }));
  expect(await screen.findByText("Member 2")).toBeTruthy();
  expect(secondPageReads).toBe(2);
});

it("revokes the pane when a member read loses its browser session", async () => {
  const authfailure = vi.fn();
  const close = vi.fn();
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ code: "unauthorized", detail: "Session expired" }, 401));
  render(ProductionReview, { session: "expired", set, draft, onrefresh: vi.fn(), onauthfailure: authfailure, onclose: close });
  await waitFor(() => expect(authfailure).toHaveBeenCalled());
  expect(close).toHaveBeenCalled();
});

it("retries a member mode change with the same operation after a lost response", async () => {
  const attempts: { operationID: string; etag: string | null; change: unknown }[] = [];
  const refresh = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    if (url.endsWith("/changes") && init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      attempts.push({ operationID: body.operation_id, etag: new Headers(init.headers).get("If-Match"), change: body.changes[0] });
      if (attempts.length === 1) throw new TypeError("synthetic lost response");
      return json({ operation_id: body.operation_id, set_id: set.id, revision: 2, etag: 5 });
    }
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: refresh, onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Use Keep selected for member 1" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  await fireEvent.click(screen.getByRole("button", { name: "Retry change" }));
  await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
  expect(attempts).toHaveLength(2);
  expect(attempts[0]).toEqual(attempts[1]);
  expect(attempts[0]?.etag).toBe("4");
  expect(attempts[0]?.change).toEqual({ kind: "mode", member_id: memberA.id, mode: "keep_selected" });
});

it("clears uncertainty with an exact Keep decision and strips server provenance", async () => {
  const refresh = vi.fn();
  let posted: unknown;
  const uncertain = { ...textFlag, label: "" };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [uncertain], next_cursor: "" });
    if (url.endsWith(`/maps/${memberA.id}?limit=65536`)) return mapPage();
    if (url.endsWith("/changes") && init?.method === "POST") {
      posted = JSON.parse(String(init.body));
      return json({ operation_id: (posted as { operation_id: string }).operation_id, set_id: set.id, revision: 2, etag: 5 });
    }
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: refresh, onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Inspect flagged text at text bytes 0–17" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Keep passage at text bytes 0–17" }));
  await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
  expect((posted as { changes: unknown[] }).changes).toEqual([{ kind: "decision", decision: {
    id: uncertain.id, member_id: memberA.id, action: "keep", uncertain: false,
    reason: uncertain.reason, label: "", selector: uncertain.selector,
  } }]);
  expect(screen.queryByText(uncertain.reason)).toBeNull();
});

it("replaces an uncertain keep with an exact redaction", async () => {
  let posted: unknown;
  const uncertain = textFlag;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [uncertain], next_cursor: "" });
    if (url.endsWith(`/maps/${memberA.id}?limit=65536`)) return mapPage();
    if (url.endsWith("/changes") && init?.method === "POST") {
      posted = JSON.parse(String(init.body));
      return json({ operation_id: (posted as { operation_id: string }).operation_id, set_id: set.id, revision: 2, etag: 5 });
    }
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Inspect flagged text at text bytes 0–17" }));
  expect((await screen.findByRole("button", { name: "Redact passage at text bytes 0–17" })).hasAttribute("disabled")).toBe(true);
  await fireEvent.input(screen.getByRole("textbox", { name: "Private reason for redaction" }), { target: { value: "New synthetic redaction basis" } });
  await fireEvent.input(screen.getByRole("textbox", { name: "Public label" }), { target: { value: "WITHHELD" } });
  await fireEvent.click(await screen.findByRole("button", { name: "Redact passage at text bytes 0–17" }));
  await waitFor(() => expect(posted).toBeDefined());
  expect((posted as { changes: unknown[] }).changes).toEqual([{ kind: "decision", decision: {
    id: uncertain.id, member_id: memberA.id, action: "redact", uncertain: false,
    reason: "New synthetic redaction basis", label: "WITHHELD", selector: uncertain.selector,
  } }]);
});

it("stops editing and hides stale records after a concurrent draft change", async () => {
  const refresh = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [], next_cursor: "" });
    if (url.endsWith("/changes") && init?.method === "POST") return json({ code: "production_revision_conflict", detail: "production revision changed" }, 409);
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: refresh, onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Use Keep selected for member 1" }));
  expect(await screen.findByText("Draft changed during review. Refresh to load the current version.")).toBeTruthy();
  expect(screen.queryByText("Member 1")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Refresh draft" }));
  expect(refresh).toHaveBeenCalledOnce();
});

it("reports a selection conflict without claiming the draft is stale", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [textFlag], next_cursor: "" });
    if (url.endsWith(`/maps/${memberA.id}?limit=65536`)) return mapPage();
    if (url.endsWith("/changes") && init?.method === "POST") return json({ code: "decision_conflict", detail: "synthetic selection overlap" }, 409);
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Inspect flagged text at text bytes 0–17" }));
  await fireEvent.input(await screen.findByRole("textbox", { name: "Private reason for redaction" }), { target: { value: "New synthetic conflict basis" } });
  await fireEvent.click(await screen.findByRole("button", { name: "Redact passage at text bytes 0–17" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic selection overlap");
  expect(screen.getByText("Member 1")).toBeTruthy();
  expect(screen.queryByText("Draft changed during review. Refresh to load the current version.")).toBeNull();
  expect(screen.getByRole("button", { name: "Keep passage at text bytes 0–17" }).hasAttribute("disabled")).toBe(false);
  expect(screen.queryByRole("button", { name: "Retry change" })).toBeNull();
});

it("requires verified source text before exposing Keep and Redact decisions", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return json(draft);
    if (url.endsWith("/members?limit=50")) return json({ items: [memberA], next_cursor: "" });
    if (url.endsWith("/decisions?limit=50&uncertain=true")) return json({ items: [textFlag], next_cursor: "" });
    if (url.endsWith(`/maps/${memberA.id}?limit=65536`)) return mapPage();
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionReview, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn(), onclose: vi.fn() });
  expect(await screen.findByText("Text bytes 0–17")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Keep passage at text bytes 0–17" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Inspect flagged text at text bytes 0–17" }));
  expect(await screen.findByText("Synthetic passage")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Keep passage at text bytes 0–17" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Redact passage at text bytes 0–17" })).toBeTruthy();
});
