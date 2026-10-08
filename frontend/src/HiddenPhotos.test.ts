import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import HiddenPhotos from "./HiddenPhotos.svelte";
import App from "./App.svelte";
import { photo } from "./photo-test-fixtures.js";
import { photoPrivacyEvent, photoRevalidationErrorEvent } from "./photos.svelte.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const state = (unlocked: boolean) => ({ configured: true, change_id: "unchanged", ...(unlocked ? { expires_at: new Date(Date.now() + 300_000).toISOString() } : {}) });
const problem = (status: number, detail: string) => new Response(JSON.stringify({ detail }), { status });
const prepare = () => { vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} }); vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000); vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800); };

it.each(["lock", "unlock"])("privacy read failure preserves pending %s and its error", async kind => {
  prepare();
  let settle!: (response: Response) => void;
  let actionSignal: AbortSignal | undefined;
  const pending = new Promise<Response>(resolve => { settle = resolve; });
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith(`/${kind}`)) { actionSignal = options?.signal ?? undefined; return pending; }
    if (url.endsWith("/photos/hidden")) return new Response(JSON.stringify(state(kind === "lock")));
    return new Response(JSON.stringify({ items: [photo(1)], total: 1 }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  if (kind === "unlock") {
    await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
    await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "synthetic" } });
  }
  await fireEvent.click(await screen.findByRole("button", { name: kind === "lock" ? "Lock" : "Unlock" }));
  await waitFor(() => expect(actionSignal).toBeDefined());
  window.dispatchEvent(new CustomEvent(photoRevalidationErrorEvent, { detail: "Temporary privacy read failure" }));
  expect(actionSignal?.aborted).toBe(false);
  window.dispatchEvent(new Event(photoPrivacyEvent));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  settle(problem(503, "Synthetic passcode action failed"));
  await screen.findByText("Synthetic passcode action failed");
});

it("preserves an unlocked selection when polling observes the same state", async () => {
  prepare();
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  let hidden = state(false);
  let failing = false;
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/unlock")) { hidden = state(true); return new Response(JSON.stringify(hidden)); }
    if (url.endsWith("/photos/hidden")) return failing ? problem(503, "Temporary state failure") : new Response(JSON.stringify(hidden));
    if (url.includes("/assets/query")) return new Response(JSON.stringify({ items: [photo(1)], total: 1 }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "synthetic" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await screen.findByText("1 selected photo");
  const listings = () => fetcher.mock.calls.filter(([url]) => url.includes("/assets/query")).length;
  const before = listings();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.getByText("1 selected photo")).not.toBeNull();
  expect(listings()).toBe(before);
  hidden = { ...hidden, change_id: "changed-by-cli" };
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(listings()).toBe(before + 1));
  await waitFor(() => expect(screen.queryByText("1 selected photo")).toBeNull());
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  const otherContext = new BroadcastChannel(photoPrivacyEvent);
  otherContext.postMessage({ origin: "synthetic-other-context" });
  otherContext.close();
  await waitFor(() => expect(listings()).toBe(before + 2));
  await waitFor(() => expect(screen.queryByText("1 selected photo")).toBeNull());
  failing = true;
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Could not check Hidden access. Retry or wait for the next check.");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
});

it.each([false, true])("Retry clears a resolved read error and preserves action errors when unlocked=%s", async unlocked => {
  prepare();
  let failing = unlocked;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/unlock")) return problem(403, "Wrong passcode");
    if (url.endsWith("/photos/hidden")) return failing ? problem(503, "Temporary read failure") : new Response(JSON.stringify(state(unlocked)));
    return new Response(JSON.stringify({ items: [], total: 0 }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  if (!unlocked) {
    await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
    await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "wrong" } });
    await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    await screen.findByText("Wrong passcode");
    failing = true;
    window.dispatchEvent(new Event(photoPrivacyEvent));
  }
  await screen.findByText("Temporary read failure");
  failing = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByText("Temporary read failure")).toBeNull());
  if (unlocked) await screen.findByRole("main", { name: "Photo library" });
  else expect(screen.getByText("Wrong passcode")).not.toBeNull();
});

it.each(["read", "action"])("routes Hidden %s HTTP 401 to the session owner", async source => {
  const expired = vi.fn();
  vi.stubGlobal("fetch", vi.fn(async (url: string) => url.endsWith("/unlock") || source === "read" ? problem(401, "Session expired") : new Response(JSON.stringify(state(false)))));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: expired });
  if (source === "action") {
    await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
    await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "synthetic" } });
    await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  }
  await waitFor(() => expect(expired).toHaveBeenCalledOnce());
});

it("poll HTTP 401 reaches the shared browser session-expired screen", async () => {
  prepare();
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("fetch", vi.fn(async (url: string) => url.endsWith("/photos/hidden") ? problem(401, "Session expired") : new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [], total: 0 }))));
  render(App);
  await screen.findByText("The browser session expired or was rejected. Run `docbank web` again.");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
});

it.each([false, true])("real Unhide retains stale revision failure with mixed success=%s", async mixed => {
  prepare();
  let items = mixed ? [photo(1), photo(2), photo(3)] : [photo(2)];
  let stale = true;
  const failure = `${mixed ? 2 : 1} photo${mixed ? "s" : ""} failed: Synthetic stale photo revision`;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) return new Response(JSON.stringify(state(true)));
    if (url.includes("/assets/query")) return new Response(JSON.stringify({ items, total: items.length }));
    if (url.endsWith("/unhide")) {
      if ((url.includes("/photo-2/") || url.includes("/photo-3/")) && stale) return problem(412, "Synthetic stale photo revision");
      items = items.filter(item => !url.includes(`/${item.asset_id}/`));
      return new Response("{}");
    }
    return new Response("{}");
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await screen.findByRole("checkbox", { name: "Select photo Photo 2.jpg" });
  if (mixed) { await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })); await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" })); await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 3.jpg" })); }
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 2.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  await screen.findByText(failure);
  await screen.findByRole("checkbox", { name: "Select photo Photo 2.jpg" });
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  await fireEvent(window, new Event(photoPrivacyEvent));
  await screen.findByRole("button", { name: "Actions for Photo 2.jpg" });
  expect(screen.getByText(failure)).not.toBeNull();
  stale = false;
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 2.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  await waitFor(() => expect(screen.queryByText(failure)).toBeNull());
});
