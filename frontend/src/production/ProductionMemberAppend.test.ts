import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ProductionMemberAppend from "./ProductionMemberAppend.svelte";
import type { ProductionDraft, ProductionPreparedMember, ProductionSet } from "./api.js";

const target: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "New production",
  creator: "test", created_at: "2026-09-26T00:00:00Z", head_revision: 1 };
const sourceSet: ProductionSet = { id: "22222222-2222-4222-8222-222222222222", name: "Prepared source",
  creator: "test", created_at: "2026-09-25T00:00:00Z", head_revision: 1 };
const versionID = "33333333-3333-4333-8333-333333333333";
const source: ProductionPreparedMember = { id: "44444444-4444-4444-8444-444444444444",
  vault_id: "55555555-5555-4555-8555-555555555555", node_id: 7, ordinal: 1,
  source_version_id: versionID, source_sha256: "a".repeat(64), source_size: 10,
  pdf_sha256: "b".repeat(64), pdf_size: 20, map_sha256: "c".repeat(64),
  page_inventory_sha256: "d".repeat(64), family: { kind: "standalone", root_version_id: versionID },
  mode: "keep_selected", reviewed: true, review_binding: "e".repeat(64) };
const draft: ProductionDraft = { set_id: target.id, revision: 1, etag: 1, state: "draft",
  membership_sealed: false, member_hash: "f".repeat(64) };
const sourceDraft: ProductionDraft = { ...draft, set_id: sourceSet.id, etag: 2, member_hash: "1".repeat(64) };

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => {
  cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it("copies a prepared source as an unreviewed first occurrence through the exact draft", async () => {
  const onrefresh = vi.fn();
  const writes: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${sourceSet.id}`) return Response.json(sourceSet);
    if (url === `/api/v1/productions/sets/${sourceSet.id}/revisions/1`) return Response.json(sourceDraft);
    if (url === `/api/v1/productions/sets/${sourceSet.id}/revisions/1/members?limit=50`)
      return Response.json({ items: [source], next_cursor: "" });
    if (url === `/api/v1/productions/sets/${target.id}/revisions/1`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${target.id}/revisions/1/members` && init?.method === "POST") {
      expect(new Headers(init.headers).get("If-Match")).toBe("1");
      writes.push(JSON.parse(String(init.body)));
      return Response.json({ operation_id: (writes[0] as {operation_id:string}).operation_id,
        set_id: target.id, revision: 1, etag: 2 });
    }
    throw new Error(`unexpected request ${url}`);
  });
  render(ProductionMemberAppend, { session: "synthetic", draft, sets: [target, sourceSet], targetMembers: [],
    targetCursor: "", onrefresh, onstale: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("combobox", { name: /Prepared source set/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Prepared source" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Add member 1 as new occurrence" }));
  await waitFor(() => expect(onrefresh).toHaveBeenCalledOnce());
  expect(writes).toHaveLength(1);
  const body = writes[0] as {operation_id:string;members:ProductionPreparedMember[]};
  expect(body.operation_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(body.members).toEqual([{ ...source, id: expect.stringMatching(/^[0-9a-f-]{36}$/), ordinal: 1,
    reviewed: false, review_binding: "" }]);
});

it("retries a lost append response with the same occurrence and operation identities", async () => {
  const writes: unknown[] = [];
  const onrefresh = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${sourceSet.id}`) return Response.json(sourceSet);
    if (url === `/api/v1/productions/sets/${sourceSet.id}/revisions/1`) return Response.json(sourceDraft);
    if (url === `/api/v1/productions/sets/${sourceSet.id}/revisions/1/members?limit=50`)
      return Response.json({ items: [source], next_cursor: "" });
    if (url === `/api/v1/productions/sets/${target.id}/revisions/1`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${target.id}/revisions/1/members` && init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      writes.push(body);
      if (writes.length === 1) throw new TypeError("synthetic lost response");
      return Response.json({ operation_id: body.operation_id, set_id: target.id, revision: 1, etag: 2 });
    }
    throw new Error(`unexpected request ${url}`);
  });
  render(ProductionMemberAppend, { session: "synthetic", draft, sets: [target, sourceSet], targetMembers: [],
    targetCursor: "", onrefresh, onstale: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("combobox", { name: /Prepared source set/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Prepared source" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Add member 1 as new occurrence" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  expect(screen.getByRole("button", { name: "Refresh draft" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry append" }));
  await waitFor(() => expect(onrefresh).toHaveBeenCalledOnce());
  expect(writes).toHaveLength(2);
  expect(writes[1]).toEqual(writes[0]);
});
