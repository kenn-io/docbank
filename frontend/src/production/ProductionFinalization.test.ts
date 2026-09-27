import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ProductionFinalization from "./ProductionFinalization.svelte";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic numbered",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 5, state: "draft",
  membership_sealed: true, member_hash: "a".repeat(64), numbering_recipe_id: "bates-sequential-v1" };
const namespace = { namespace_id: "22222222-2222-4222-8222-222222222222", prefix: "PROD",
  suffix: "", padding: 6, created_at: "2026-09-27T00:00:00Z" };

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => {
  cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it("finalizes a sealed numbered draft using an existing namespace", async () => {
  const onrefresh = vi.fn();
  let command: { operation_id: string; namespace_id: string; snapshot_id: string; start_at: number } | null = null;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/bates/namespaces?limit=100")
      return Response.json({ items: [namespace], total: 1 });
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/finalize` && init?.method === "POST") {
      expect(new Headers(init.headers).get("If-Match")).toBe("5");
      command = JSON.parse(String(init.body));
      return Response.json({ draft: { ...draft, state: "finalized" }, ...command,
        prepared_sha256: "b".repeat(64), receipt_sha256: "c".repeat(64) });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionFinalization, { session: "synthetic", set, draft, onrefresh, onauthfailure: vi.fn() });
  await waitFor(() => expect(screen.getByRole("combobox", { name: /Bates namespace/ })).not.toHaveProperty("disabled", true));
  await fireEvent.click(await screen.findByRole("combobox", { name: /Bates namespace/ }));
  await fireEvent.click(screen.getByRole("option", { name: /PROD/ }));
  await fireEvent.click(screen.getByRole("checkbox", { name: /reviewed.*ready to finalize/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Finalize production" }));
  await waitFor(() => expect(onrefresh).toHaveBeenCalledOnce());
  expect(command).toMatchObject({ namespace_id: namespace.namespace_id, start_at: 0 });
  expect(command!.operation_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(command!.snapshot_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(screen.getByText("Finalized revision 2")).toBeTruthy();
});

it("retries a lost finalization response with the same command identities", async () => {
  const commands: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/bates/namespaces?limit=100")
      return Response.json({ items: [namespace], total: 1, next_cursor: "" });
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/finalize` && init?.method === "POST") {
      const command = JSON.parse(String(init.body)); commands.push(command);
      if (commands.length === 1) throw new TypeError("synthetic lost response");
      return Response.json({ draft: { ...draft, state: "finalized" }, ...command,
        prepared_sha256: "b".repeat(64), receipt_sha256: "c".repeat(64) });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionFinalization, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn() });
  await waitFor(() => expect(screen.getByRole("combobox", { name: /Bates namespace/ })).not.toHaveProperty("disabled", true));
  await fireEvent.click(await screen.findByRole("combobox", { name: /Bates namespace/ }));
  await fireEvent.click(screen.getByRole("option", { name: /PROD/ }));
  await fireEvent.click(screen.getByRole("checkbox", { name: /reviewed.*ready to finalize/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Finalize production" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  await fireEvent.click(screen.getByRole("button", { name: "Retry finalization" }));
  await waitFor(() => expect(commands).toHaveLength(2));
  expect(commands[1]).toEqual(commands[0]);
});

it("releases a stale preflight so the operator can refresh before a new attempt", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input); requests.push(url);
    if (url === "/api/v1/bates/namespaces?limit=100") return Response.json({ items: [namespace], total: 1 });
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json({ ...set, head_revision: 3 });
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionFinalization, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn() });
  await waitFor(() => expect(screen.getByRole("combobox", { name: /Bates namespace/ })).not.toHaveProperty("disabled", true));
  await fireEvent.click(screen.getByRole("combobox", { name: /Bates namespace/ }));
  await fireEvent.click(screen.getByRole("option", { name: /PROD/ }));
  await fireEvent.click(screen.getByRole("checkbox", { name: /reviewed.*ready to finalize/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Finalize production" }));
  expect((await screen.findByRole("alert")).textContent).toContain("changed");
  expect(screen.queryByRole("button", { name: "Retry finalization" })).toBeNull();
  expect(screen.getByRole("button", { name: "Refresh draft" })).toBeTruthy();
  expect(requests).not.toContain(`/api/v1/productions/sets/${set.id}/revisions/2/finalize`);
});

it("releases a server revision conflict after preflight so it can be refreshed", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url === "/api/v1/bates/namespaces?limit=100") return Response.json({ items: [namespace], total: 1 });
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/finalize`)
      return Response.json({ title: "Conflict", detail: "draft changed" }, { status: 409 });
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionFinalization, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn() });
  await waitFor(() => expect(screen.getByRole("combobox", { name: /Bates namespace/ })).not.toHaveProperty("disabled", true));
  await fireEvent.click(screen.getByRole("combobox", { name: /Bates namespace/ }));
  await fireEvent.click(screen.getByRole("option", { name: /PROD/ }));
  await fireEvent.click(screen.getByRole("checkbox", { name: /reviewed.*ready to finalize/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Finalize production" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Retry finalization" })).toBeNull();
  expect(screen.getByRole("button", { name: "Refresh draft" })).toBeTruthy();
});
