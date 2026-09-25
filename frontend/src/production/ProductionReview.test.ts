import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ProductionReview from "./ProductionReview.svelte";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic review", creator: "test", created_at: "2026-09-25T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 4, state: "draft", membership_sealed: false };
const member = (id: string, ordinal: number) => ({ id, ordinal, node_id: ordinal + 10, source_version_id: `00000000-0000-4000-8000-${String(ordinal).padStart(12, "0")}`, mode: "redact_selected", reviewed: false });
const memberA = member("22222222-2222-4222-8222-222222222222", 1);
const memberB = member("33333333-3333-4333-8333-333333333333", 2);
const flag = (id: string, memberID: string, page: number) => ({ id, member_id: memberID, action: "keep", uncertain: true,
  selector: { kind: "page", map_sha256: "a".repeat(64), pages: [page] } });
const flagA = flag("44444444-4444-4444-8444-444444444444", memberA.id, 1);
const flagB = flag("55555555-5555-4555-8555-555555555555", memberB.id, 2);

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
