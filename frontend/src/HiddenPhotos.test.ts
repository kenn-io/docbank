import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import HiddenPhotos from "./HiddenPhotos.svelte";
import App from "./App.svelte";
import { photo } from "./photo-test-fixtures.js";
import { photoPrivacyEvent } from "./photos.svelte.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const state = (unlocked: boolean) => ({ configured: true, change_id: "unchanged", ...(unlocked ? { expires_at: new Date(Date.now() + 300_000).toISOString() } : {}) });
const problem = (status: number, detail: string) => new Response(JSON.stringify({ detail }), { status });
const prepare = () => { vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} }); vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000); vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800); };

it.each([false, true])("Retry clears a resolved read error when unlocked=%s", async unlocked => {
  prepare();
  let failing = true;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) return failing ? problem(503, "Temporary read failure") : new Response(JSON.stringify(state(unlocked)));
    return new Response(JSON.stringify({ items: [], total: 0 }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await screen.findByRole("alert");
  failing = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  if (unlocked) await screen.findByRole("main", { name: "Photo library" });
  else await screen.findByRole("button", { name: "Unlock" });
});

it("successful recovery preserves a failed passcode action", async () => {
  prepare();
  let failing = false;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/unlock")) return problem(403, "Wrong passcode");
    if (url.endsWith("/photos/hidden")) return failing ? problem(503, "Temporary read failure") : new Response(JSON.stringify(state(false)));
    return new Response("{}");
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "wrong" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText("Wrong passcode");
  failing = true;
  window.dispatchEvent(new Event(photoPrivacyEvent));
  await screen.findByText("Temporary read failure");
  failing = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByText("Temporary read failure")).toBeNull());
  expect(screen.getByText("Wrong passcode")).not.toBeNull();
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
