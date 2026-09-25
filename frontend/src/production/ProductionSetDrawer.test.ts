import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import ProductionSetDrawer from "./ProductionSetDrawer.svelte";

const first = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic review A", creator: "test", created_at: "2026-09-25T00:00:00Z", head_revision: 1 };
const second = { id: "22222222-2222-4222-8222-222222222222", name: "Synthetic review B", creator: "test", created_at: "2026-09-25T00:01:00Z", head_revision: 2 };

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

it("traverses bounded set pages and inspects the exact head draft", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/productions/sets?limit=50") return json({ items: [first], next_cursor: "next-page" });
    if (url === "/api/v1/productions/sets?limit=50&cursor=next-page") return json({ items: [second], next_cursor: "" });
    if (url === `/api/v1/productions/sets/${second.id}/revisions/2`) return json({ set_id: second.id, revision: 2, etag: 4, state: "draft", membership_sealed: false });
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionSetDrawer, { session: "synthetic-session", onclose: vi.fn(), onauthfailure: vi.fn() });
  const drawer = await screen.findByRole("dialog", { name: "Production sets" });
  expect(await within(drawer).findByRole("button", { name: "Synthetic review A" })).toBeTruthy();
  await fireEvent.click(within(drawer).getByRole("button", { name: "Load more sets" }));
  await fireEvent.click(await within(drawer).findByRole("button", { name: "Synthetic review B" }));
  expect(await within(drawer).findByText("Draft revision 2")).toBeTruthy();
  expect(within(drawer).getByText("Membership open")).toBeTruthy();
  expect(requests).toContain(`/api/v1/productions/sets/${second.id}/revisions/2`);
});

it("reuses the exact operation after an uncertain create response", async () => {
  const creates: { id: string; name: string; instructions: string }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    if (url === "/api/v1/productions/sets?limit=50") return json({ items: [], next_cursor: "" });
    if (url === "/api/v1/productions/sets" && init.method === "POST") {
      const body = JSON.parse(String(init.body)) as { operation_id: string; name: string; instructions: string };
      creates.push({ id: body.operation_id, name: body.name, instructions: body.instructions });
      if (creates.length === 1) throw new TypeError("synthetic lost response");
      return json({ set: first, draft: { set_id: first.id, revision: 1, etag: 1, state: "draft", membership_sealed: false } }, 201);
    }
    throw new Error(`unexpected request ${url}`);
  });

  render(ProductionSetDrawer, { session: "synthetic-session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByText("No production sets yet");
  await fireEvent.input(screen.getByRole("textbox", { name: "Set name" }), { target: { value: "Synthetic review A" } });
  await fireEvent.input(screen.getByRole("textbox", { name: "Instructions" }), { target: { value: "Review selected synthetic pages." } });
  await fireEvent.click(screen.getByRole("button", { name: "Create draft" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  await fireEvent.click(screen.getByRole("button", { name: "Retry create" }));
  expect(await screen.findByText("Draft revision 1")).toBeTruthy();
  expect(creates).toHaveLength(2);
  expect(creates[1]).toEqual(creates[0]);
  expect(creates[0].id).toMatch(/^[0-9a-f-]{36}$/);
});

it("revokes the browser view on an unauthorized set read", async () => {
  const close = vi.fn();
  const authfailure = vi.fn();
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ code: "unauthorized", detail: "Session expired" }, 401));
  render(ProductionSetDrawer, { session: "expired-session", onclose: close, onauthfailure: authfailure });
  await waitFor(() => expect(authfailure).toHaveBeenCalledOnce());
  expect(close).toHaveBeenCalledOnce();
});
