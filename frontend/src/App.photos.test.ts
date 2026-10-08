import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { photo, storage } from "./photo-test-fixtures.js";
import App from "./App.svelte";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("reloads photos when the first privacy check observes a concurrent Hide", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let resolveState!: (response: Response) => void;
  let stateChecks = 0;
  let items = [photo(1)];
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) { stateChecks++; return new Promise<Response>(resolve => { resolveState = resolve; }); }
    if (url.includes("/assets/query")) return new Response(JSON.stringify({ items, total: items.length }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  }));
  render(App);
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  items = [photo(2)];
  resolveState(new Response(JSON.stringify({ change_id: "after-cli-hide", configured: true })));
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull());
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(stateChecks).toBe(2));
  resolveState(new Response(JSON.stringify({ change_id: "after-cli-hide", configured: true })));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.getByText("1 selected photo")).toBeTruthy();
});

it.each([false, true])("retains Hide errors through polling and clears them after successful retry, mixed=%s", async mixed => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let items = mixed ? [photo(1), photo(2)] : [photo(2)];
  let stale = true;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) return new Response(JSON.stringify({ change_id: String(items.length), configured: true }));
    if (url.includes("/assets/query")) return new Response(JSON.stringify({ items, total: items.length }));
    if (url.endsWith("/hide")) {
      if (url.includes("/photo-2/") && stale) return new Response(JSON.stringify({ detail: "Synthetic stale photo revision" }), { status: 412 });
      items = items.filter(item => !url.includes(`/${item.asset_id}/`));
      return new Response("{}");
    }
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  }));
  render(App);
  if (mixed) await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 2.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Hide" }));
  const failure = "1 photo failed: Synthetic stale photo revision";
  await screen.findAllByText(failure);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.getAllByText(failure).length).toBeGreaterThan(0);
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  expect(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" })).not.toBeNull();
  stale = false;
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 2.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Hide" }));
  await waitFor(() => expect(screen.queryAllByText(failure)).toHaveLength(0));
});

it("retains photo state and previews across sidebar switches until lock", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  const stored = storage();
  let pollFailed = false;
  let previewDenied = false;
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden") && pollFailed) throw new Error("Temporary state failure");
    if (url.endsWith("/photos/hidden")) return new Response(JSON.stringify({ change_id: "initial", configured: true }));
    if (url.includes("/photos/assets/query")) return new Response(JSON.stringify({ items: [{ ...photo(1), previews: { ...photo(1).previews, grid: { state: "ready", generation_id: "synthetic" } } }], total: 1 }));
    if (url.includes("/previews/") && previewDenied) { previewDenied = false; return new Response(JSON.stringify({ detail: "Hidden photos are locked" }), { status: 403 }); }
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await screen.findByRole("main", { name: "Photo library" });
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  const cacheName = stored.open.mock.calls[0][0];
  await waitFor(() => expect(stored.data.get(cacheName)?.size).toBe(1));
  const listings = () => fetcher.mock.calls.filter(([url]) => url.includes("/photos/assets/query")).length;
  const previews = () => fetcher.mock.calls.filter(([url]) => url.includes("/previews/")).length;
  expect(listings()).toBe(2);
  expect(previews()).toBe(1);
  const historyLength = history.length;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(history.length).toBe(historyLength);
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(location.pathname).toBe("/");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await screen.findByText("1 selected photo");
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(listings()).toBe(2);
  expect(previews()).toBe(1);
  const states = () => fetcher.mock.calls.filter(([url]) => url.endsWith("/photos/hidden")).length;
  await new Promise(resolve => setTimeout(resolve, 0));
  const beforeForeground = states();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(states()).toBe(beforeForeground + 1));
  expect(listings()).toBe(2);
  await new Promise(resolve => setTimeout(resolve, 0));
  pollFailed = true;
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByRole("alert");
  expect(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })).not.toBeNull();
  expect(stored.data.get(cacheName)?.size).toBe(1);
  expect(listings()).toBe(2);
  pollFailed = false;
  await new Promise(resolve => setTimeout(resolve, 0));
  const recovered = vi.fn();
  window.addEventListener("docbank-photo-privacy", recovered, { once: true });
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(recovered).toHaveBeenCalledOnce());
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  previewDenied = true;
  window.dispatchEvent(new Event("docbank-photo-privacy"));
  await waitFor(() => expect(previews()).toBeGreaterThan(2));
  expect(screen.getByRole("main", { name: "Photo library" })).not.toBeNull();
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  history.replaceState(null, "", "/");
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: /Lock/ }));
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
});
