import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it("opens retained production sets from the vault toolbar", async () => {
  history.replaceState(null, "", "/#web_session=synthetic-session&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const root = { id: 1, name: "", kind: "dir", size: 0, revision: 1,
    created_at: "2026-09-25T00:00:00Z", modified_at: "2026-09-25T00:00:00Z", path: "/" };
  const set = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic production",
    creator: "test", created_at: "2026-09-25T00:00:00Z", head_revision: 1 };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    let body: unknown;
    if (url === "/api/v1/path?path=%2F") body = root;
    else if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") body = { directory: root, items: [], total: 0, limit: 1000, offset: 0 };
    else if (url === "/api/v1/tags?limit=1000&offset=0") body = { items: [], total: 0, limit: 1000, offset: 0 };
    else if (url === "/api/v1/productions/sets?limit=50") body = { items: [set], next_cursor: "" };
    else body = {};
    return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
  });

  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Production sets" }));
  const drawer = await screen.findByRole("dialog", { name: "Production sets" });
  expect(await within(drawer).findByRole("button", { name: "Synthetic production" })).toBeTruthy();
});
