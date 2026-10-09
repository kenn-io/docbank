import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import HiddenPhotos from "./HiddenPhotos.svelte";
import App from "./App.svelte";
import { photo } from "./photo-test-fixtures.js";
import { photoPrivacyEvent, photoRevalidationErrorEvent } from "./photos.svelte.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const state = (unlocked: boolean) => ({ configured: true, change_id: "unchanged", ...(unlocked ? { expires_at: new Date(Date.now() + 300_000).toISOString() } : {}) });
const problem = (status: number, detail: string, code = "") => new Response(JSON.stringify({ detail, code }), { status });
const prepare = () => { vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} }); vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000); vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800); };

it("associates incorrect unlock feedback with its field and preserves it through privacy refresh", async () => {
  vi.stubGlobal("fetch", vi.fn(async (url: string) => url.endsWith("/unlock") ? problem(403, "Wrong passcode", "hidden_passcode") : new Response(JSON.stringify(state(false)))));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  const passcode = await screen.findByLabelText("Passcode", { exact: true });
  await waitFor(() => expect((passcode as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(passcode, { target: { value: "wrong" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText("Incorrect passcode.");
  expect(passcode.getAttribute("aria-invalid")).toBe("true");
  expect(passcode.getAttribute("aria-describedby")).toBe("hidden-passcode-error");
  await waitFor(() => expect(document.activeElement).toBe(passcode));
  await fireEvent(window, new Event(photoPrivacyEvent));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.getByText("Incorrect passcode.")).toBeTruthy();
  await fireEvent.click(screen.getByText("Manage passcode"));
  await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "current" } });
  expect(screen.getByText("Incorrect passcode.")).toBeTruthy();
  await fireEvent.input(passcode, { target: { value: "edited" } });
  expect(screen.queryByText("Incorrect passcode.")).toBeNull();
});

it("keeps management credentials independent and lets Disable ignore an invalid new passcode", async () => {
  let configured = true;
  let currentWrong = false;
  const requests: unknown[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/change")) { requests.push(JSON.parse(String(options?.body))); return currentWrong ? problem(403, "Wrong current", "hidden_passcode") : problem(422, "Invalid new", "invalid_hidden_passcode"); }
    if (url.endsWith("/disable")) { requests.push(JSON.parse(String(options?.body))); configured = false; return new Response("{}"); }
    return new Response(JSON.stringify({ configured, change_id: "unchanged" }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await screen.findByRole("button", { name: "Unlock" });
  await fireEvent.click(screen.getByText("Manage passcode"));
  const current = screen.getByLabelText("Current passcode", { exact: true });
  const next = screen.getByLabelText("New passcode", { exact: true });
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  expect(requests).toHaveLength(0);
  expect(document.activeElement).toBe(next);
  await fireEvent.input(current, { target: { value: "current" } });
  await fireEvent.input(next, { target: { value: "é".repeat(513) } });
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  await screen.findByText("Use 1 to 1,024 bytes.");
  expect(next.getAttribute("aria-invalid")).toBe("true");
  expect(current.getAttribute("aria-invalid")).toBeNull();
  expect(requests).toHaveLength(0);
  await fireEvent.input(next, { target: { value: "new" } });
  currentWrong = true;
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  await screen.findByText("Incorrect passcode.");
  expect(current.getAttribute("aria-describedby")).toBe("hidden-current-passcode-error");
  await fireEvent.input(next, { target: { value: "x".repeat(1025) } });
  expect(screen.getByText("Incorrect passcode.")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Disable Hidden" }));
  await screen.findByRole("button", { name: "Set passcode" });
  expect(requests.at(-1)).toEqual({ passcode: "current" });
  expect(screen.queryByText("Incorrect passcode.")).toBeNull();
  expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).value).toBe("");
});

it.each(["setup", "unlock", "current", "new"].flatMap(field => ["x".repeat(1025), "é".repeat(513)].map(value => ({ field, value }))))("rejects oversized $field credentials before sending a request", async ({ field, value }) => {
  const mutations: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (options?.method === "POST") mutations.push(url);
    return new Response(JSON.stringify({ configured: field !== "setup", change_id: "" }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  let input: HTMLElement;
  if (field === "setup" || field === "unlock") {
    input = await screen.findByLabelText("Passcode", { exact: true });
    await waitFor(() => expect((input as HTMLInputElement).disabled).toBe(false));
    await fireEvent.input(input, { target: { value } });
    await fireEvent.click(screen.getByRole("button", { name: field === "setup" ? "Set passcode" : "Unlock" }));
  } else {
    await screen.findByRole("button", { name: "Unlock" });
    await fireEvent.click(screen.getByText("Manage passcode"));
    input = screen.getByLabelText(field === "current" ? "Current passcode" : "New passcode", { exact: true });
    await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: field === "current" ? value : "current" } });
    if (field === "new") await fireEvent.input(input, { target: { value } });
    await fireEvent.click(screen.getByRole("button", { name: field === "current" ? "Disable Hidden" : "Change passcode" }));
  }
  await screen.findByText("Use 1 to 1,024 bytes.");
  expect(input.getAttribute("aria-invalid")).toBe("true");
  expect(document.activeElement).toBe(input);
  expect(mutations).toHaveLength(0);
});

it.each(["x".repeat(1024), "é".repeat(512)])("sends a valid 1,024-byte passcode", async value => {
  const mutations: unknown[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/unlock")) { mutations.push(JSON.parse(String(options?.body))); return problem(503, "Synthetic unavailable", "validation"); }
    return new Response(JSON.stringify(state(false)));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  const input = await screen.findByLabelText("Passcode", { exact: true });
  await waitFor(() => expect((input as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(input, { target: { value } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByRole("alert");
  expect(mutations).toEqual([{ passcode: value }]);
  expect(input.getAttribute("aria-invalid")).toBeNull();
});

it("shows lockout beside the submitted form without marking its field invalid", async () => {
  let locked = false;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/unlock")) { locked = true; return problem(429, "Locked", "hidden_lockout"); }
    return new Response(JSON.stringify({ ...state(false), ...(locked ? { locked_until: new Date(Date.now() + 300_000).toISOString() } : {}) }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  const passcode = await screen.findByLabelText("Passcode", { exact: true });
  await waitFor(() => expect((passcode as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(passcode, { target: { value: "wrong" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText(/Too many attempts/);
  expect(screen.getAllByText(/Too many attempts/)).toHaveLength(1);
  expect(passcode.getAttribute("aria-invalid")).toBeNull();
});

it("waits for the initial privacy state and focuses locally rejected empty credentials", async () => {
  let resolve!: (response: Response) => void;
  const fetcher = vi.fn(() => new Promise<Response>(settle => { resolve = settle; }));
  vi.stubGlobal("fetch", fetcher);
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  expect(screen.queryByRole("button", { name: "Set passcode" })).toBeNull();
  resolve(new Response(JSON.stringify({ configured: false, change_id: "" })));
  await fireEvent.click(await screen.findByRole("button", { name: "Set passcode" }));
  await screen.findByText("Enter a passcode.");
  expect(document.activeElement).toBe(screen.getByLabelText("Passcode", { exact: true }));
  expect(fetcher).toHaveBeenCalledOnce();
});

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
  window.dispatchEvent(new CustomEvent(photoPrivacyEvent, { detail: state(kind === "lock") }));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  settle(problem(503, "Synthetic passcode action failed"));
  await screen.findByText("Synthetic passcode action failed");
});

it("applies fresh privacy state without another GET and ignores an older aborted result", async () => {
  prepare();
  let settle!: (response: Response) => void;
  let readSignal: AbortSignal | undefined;
  let reads = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) { reads++; readSignal = options?.signal ?? undefined; return new Promise<Response>(resolve => { settle = resolve; }); }
    return new Response(JSON.stringify({ items: [photo(1)], total: 1 }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent(window, new CustomEvent(photoRevalidationErrorEvent, { detail: "Temporary read failure" }));
  await screen.findByText("Temporary read failure");
  await fireEvent(window, new CustomEvent(photoPrivacyEvent, { detail: state(true) }));
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(reads).toBe(1);
  expect(readSignal?.aborted).toBe(true);
  expect(screen.queryByText("Temporary read failure")).toBeNull();
  settle(new Response(JSON.stringify(state(false))));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.getByRole("button", { name: "Lock" })).toBeTruthy();
});

it.each([false, true])("keeps a delayed photo action failure in its originating view, hidden=%s", async hidden => {
  prepare();
  history.replaceState(null, "", `${hidden ? "/photos/hidden" : "/photos"}#web_session=synthetic&web_upload_secret=proof`);
  let settle!: (response: Response) => void;
  let requested = false;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith(hidden ? "/unhide" : "/hide")) { requested = true; return new Promise<Response>(resolve => { settle = resolve; }); }
    if (url.endsWith("/photos/hidden")) return new Response(JSON.stringify(state(true)));
    if (url.includes("/assets/query")) return new Response(JSON.stringify({ items: [photo(JSON.parse(String(options?.body)).hidden ? 1 : 2)], total: 1 }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: `Actions for Photo ${hidden ? 1 : 2}.jpg` }));
  await fireEvent.click(screen.getByRole("menuitem", { name: hidden ? "Unhide" : "Hide" }));
  await waitFor(() => expect(requested).toBe(true));
  await fireEvent.click(screen.getByRole("button", { name: hidden ? "Library" : "Hidden", exact: true }));
  await screen.findByRole("checkbox", { name: `Select photo Photo ${hidden ? 2 : 1}.jpg` });
  settle(problem(412, "Synthetic delayed revision failure"));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(screen.queryByText(/Synthetic delayed revision failure/)).toBeNull();
  if (!hidden) {
    await fireEvent(window, new CustomEvent(photoPrivacyEvent, { detail: { error: "", hidden: true } }));
    await fireEvent(window, new CustomEvent(photoPrivacyEvent, { detail: state(true) }));
    await fireEvent.click(screen.getByRole("button", { name: "Library", exact: true }));
    await screen.findByText("1 photo failed: Synthetic delayed revision failure");
  }
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
    expect(screen.getByLabelText("Passcode", { exact: true }).getAttribute("aria-invalid")).toBeNull();
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
  await fireEvent(window, new CustomEvent(photoPrivacyEvent, { detail: { error: "", hidden: false } }));
  await fireEvent(window, new CustomEvent(photoPrivacyEvent, { detail: state(true) }));
  await screen.findByRole("button", { name: "Actions for Photo 2.jpg" });
  expect(screen.getByText(failure)).not.toBeNull();
  stale = false;
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 2.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  await waitFor(() => expect(screen.queryByText(failure)).toBeNull());
});
