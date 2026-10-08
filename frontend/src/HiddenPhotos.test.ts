import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import HiddenPhotos from "./HiddenPhotos.svelte";
import App from "./App.svelte";
import { photoPrivacyEvent } from "./photos.svelte.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const state = (unlocked: boolean) => ({ configured: true, change_id: "unchanged", ...(unlocked ? { expires_at: new Date(Date.now() + 300_000).toISOString() } : {}) });
const problem = (status: number, detail: string) => new Response(JSON.stringify({ detail }), { status });
const prepare = () => { vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} }); vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000); };

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
