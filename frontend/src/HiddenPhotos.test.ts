import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import HiddenPhotos from "./HiddenPhotos.svelte";
import { photo, storage } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const state = (unlocked: boolean) => ({ configured: true, ...(unlocked ? { expires_at: new Date(Date.now() + 300_000).toISOString() } : {}) });
const prepare = () => { vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} }); vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000); vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800); };
const problem = (status: number, detail: string, code = "") => new Response(JSON.stringify({ detail, code }), { status });

it.each(["setup", "unlock", "current", "new"])("preserves %s input focus and value during an access poll", async field => {
  let poll!: () => void;
  const interval = globalThis.setInterval;
  vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
    if (delay === 2000) poll = callback as () => void;
    return interval(callback, delay, ...args);
  });
  let settle!: (response: Response) => void;
  let reads = 0;
  const hidden = { configured: field !== "setup" };
  vi.stubGlobal("fetch", vi.fn(async () => ++reads === 1 ? Response.json(hidden) : new Promise<Response>(resolve => settle = resolve)));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await screen.findByLabelText("Passcode", { exact: true });
  if (field === "setup") expect(screen.getByText("Unlocks for five minutes.")).toBeTruthy();
  else expect(screen.getByText("docbank photos hidden reset").parentElement?.textContent).toBe("Forgotten passcode? Run docbank photos hidden reset.");
  if (field === "current" || field === "new") await fireEvent.click(screen.getByText("Manage passcode"));
  const label = field === "current" ? "Current passcode" : field === "new" ? "New passcode" : "Passcode";
  const input = screen.getByLabelText(label, { exact: true }) as HTMLInputElement;
  input.focus();
  await fireEvent.input(input, { target: { value: "part" } });
  poll();
  await fireEvent.input(input, { target: { value: "partial" } });
  expect(input.disabled).toBe(false);
  expect(document.activeElement).toBe(input);
  expect(input.value).toBe("partial");
  settle(Response.json(hidden));
  await new Promise(resolve => setTimeout(resolve, 0));
  await fireEvent.input(input, { target: { value: "partial passcode" } });
  expect(screen.getByLabelText(label, { exact: true })).toBe(input);
  expect(document.activeElement).toBe(input);
  expect(input.value).toBe("partial passcode");
});

it("associates incorrect unlock feedback with its field and preserves it through access check", async () => {
  let settle!: (response: Response) => void;
  let initial = true;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => url.endsWith("/unlock") ? problem(403, "Wrong passcode", "hidden_passcode") : initial ? (initial = false, new Promise<Response>(resolve => settle = resolve)) : Response.json(state(false))));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull();
  settle(Response.json(state(false)));
  const passcode = await screen.findByLabelText("Passcode", { exact: true });
  await waitFor(() => expect((passcode as HTMLInputElement).disabled).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText("Enter a passcode.");
  expect(document.activeElement).toBe(passcode);
  await fireEvent.input(passcode, { target: { value: "wrong" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText("Incorrect passcode.");
  expect(passcode.getAttribute("aria-invalid")).toBe("true");
  expect(passcode.getAttribute("aria-describedby")).toBe("hidden-passcode-error");
  await waitFor(() => expect(document.activeElement).toBe(passcode));
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await new Promise(resolve => setTimeout(resolve, 0));
  await fireEvent.click(screen.getByText("Manage passcode"));
  await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "current" } });
  expect(screen.getByText("Incorrect passcode.")).toBeTruthy();
  await fireEvent.input(passcode, { target: { value: "edited" } });
  expect(screen.queryByText("Incorrect passcode.")).toBeNull();
});

it("keeps management credentials independent and lets Disable ignore an invalid new passcode", async () => {
  let configured = true;
  const requests: unknown[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/change")) { requests.push(JSON.parse(String(options?.body))); return problem(403, "Wrong current", "hidden_passcode"); }
    if (url.endsWith("/disable")) { requests.push(JSON.parse(String(options?.body))); configured = false; return new Response("{}"); }
    return new Response(JSON.stringify({ configured }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await screen.findByRole("button", { name: "Unlock" });
  await fireEvent.click(screen.getByText("Manage passcode"));
  const current = screen.getByLabelText("Current passcode", { exact: true });
  const next = screen.getByLabelText("New passcode", { exact: true });
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  expect(requests).toHaveLength(0);
  expect(document.activeElement).toBe(current);
  expect(current.getAttribute("aria-invalid")).toBe("true");
  expect(next.getAttribute("aria-invalid")).toBe("true");
  await fireEvent.input(current, { target: { value: "current" } });
  await fireEvent.input(next, { target: { value: "new" } });
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  await screen.findByText("Incorrect passcode.");
  expect(current.getAttribute("aria-describedby")).toBe("hidden-current-passcode-error");
  await fireEvent.input(next, { target: { value: "x".repeat(1025) } });
  await waitFor(() => expect(screen.getByRole("button", { name: "Disable Hidden" }).hasAttribute("disabled")).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Disable Hidden" }));
  await screen.findByRole("button", { name: "Set passcode" });
  expect(requests.at(-1)).toEqual({ passcode: "current" });
  expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).value).toBe("");
});

it.each([false, true])("notifies Library when Disable finishes after unmount, lost reply=%s", async lost => {
  let settle!: (response: Response) => void;
  let reject!: (cause: Error) => void;
  let signal: AbortSignal | undefined;
  const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/disable")) { signal = options?.signal ?? undefined; return new Promise<Response>((resolve, fail) => { settle = resolve; reject = fail; }); }
    return Response.json(state(false));
  });
  vi.stubGlobal("fetch", fetcher);
  const onunhidden = vi.fn();
  const view = render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn(), onunhidden });
  await screen.findByRole("button", { name: "Unlock" });
  await fireEvent.click(screen.getByText("Manage passcode"));
  await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "synthetic" } });
  await fireEvent.click(screen.getByRole("button", { name: "Disable Hidden" }));
  expect(signal).toBeDefined();
  view.unmount();
  expect(signal?.aborted).toBe(false);
  if (lost) reject(new TypeError("Reply lost after commit")); else settle(Response.json({}));
  await waitFor(() => expect(onunhidden).toHaveBeenCalledOnce());
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it.each(["setup", "unlock", "current", "new"])("bounds $0 passcodes before sending them", async field => {
  const mutations: unknown[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (options?.method === "POST") { mutations.push(JSON.parse(String(options.body))); return problem(503, "Unavailable"); }
    return Response.json({ configured: field !== "setup" });
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  const label = field === "current" ? "Current passcode" : field === "new" ? "New passcode" : "Passcode";
  await waitFor(() => expect(screen.getByRole("button", { name: field === "setup" ? "Set passcode" : "Unlock" }).hasAttribute("disabled")).toBe(false));
  if (field === "current" || field === "new") { await fireEvent.click(screen.getByText("Manage passcode")); await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "current" } }); }
  const input = screen.getByLabelText(label, { exact: true });
  const submit = () => fireEvent.click(screen.getByRole("button", { name: field === "setup" ? "Set passcode" : field === "unlock" ? "Unlock" : field === "current" ? "Disable Hidden" : "Change passcode" }));
  for (const value of ["x".repeat(1025), "é".repeat(513)]) {
    await fireEvent.input(input, { target: { value } }); await submit();
    await screen.findByText("Use 1 to 1,024 bytes.");
    expect(input.getAttribute("aria-invalid")).toBe("true"); expect(document.activeElement).toBe(input);
    expect(mutations).toHaveLength(0);
  }
  await fireEvent.input(input, { target: { value: "é".repeat(512) } }); await submit();
  await screen.findByText("Unavailable");
  expect(mutations).toHaveLength(1); expect(input.getAttribute("aria-invalid")).toBeNull();
});

it("keeps one associated lockout visible outside management until its original deadline", async () => {
  let locked = false;
  let now = Date.now();
  vi.spyOn(Date, "now").mockImplementation(() => now);
  const deadline = new Date(now + 300_000).toISOString();
  let reads = 0;
  let readFailed = false;
  const rejection = "Too many attempts. Try again after the synthetic deadline.";
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/unlock")) { readFailed = true; return problem(429, rejection, "hidden_lockout"); }
    if (readFailed) throw new Error("Synthetic state read failed");
    if (url.endsWith("/change")) { locked = true; return problem(429, "Locked", "hidden_lockout"); }
    reads++;
    return new Response(JSON.stringify({ ...state(false), ...(locked ? { locked_until: deadline } : {}) }));
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  const passcode = await screen.findByLabelText("Passcode", { exact: true });
  await waitFor(() => expect((passcode as HTMLInputElement).disabled).toBe(false));
  await fireEvent.input(passcode, { target: { value: "wrong" } });
  await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  await screen.findByText("Synthetic state read failed");
  expect(screen.getByRole("status").textContent).toBe(rejection);
  expect(passcode.getAttribute("aria-invalid")).toBeNull();
  for (const form of document.querySelectorAll(".hidden-gate form")) expect(form.getAttribute("aria-describedby")).toBe("hidden-lockout");
  for (const name of ["Unlock", "Change passcode", "Disable Hidden"]) expect(screen.getByRole("button", { name, hidden: true }).getAttribute("aria-describedby")).toBe("hidden-lockout");
  readFailed = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
  locked = true;
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText(/Too many attempts/);
  await fireEvent.click(screen.getByText("Manage passcode"));
  await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "wrong" } });
  await fireEvent.input(screen.getByLabelText("New passcode", { exact: true }), { target: { value: "new" } });
  await fireEvent.click(screen.getByRole("button", { name: "Change passcode" }));
  await screen.findByText(/Too many attempts/);
  await waitFor(() => expect(screen.queryByRole("status", { name: "Loading" })).toBeNull());
  const notice = screen.getByText(/Too many attempts/);
  expect(notice.closest("details")).toBeNull();
  const details = screen.getByText("Manage passcode").closest("details")!;
  details.open = false;
  const controls = ["Unlock", "Change passcode", "Disable Hidden"].map(name => screen.getByRole("button", { name, hidden: true }));
  const forms = document.querySelectorAll(".hidden-gate form");
  const before = reads;
  now += 300_001;
  await waitFor(() => expect(screen.queryByText(/Too many attempts/)).toBeNull());
  for (const control of controls) expect(control.getAttribute("aria-describedby")).toBeNull();
  for (const form of forms) expect(form.getAttribute("aria-describedby")).toBeNull();
  expect(reads).toBe(before);
});

it.each(["lock", "revoke", "expire", "denial", "trash denial", "read"])("clears Hidden rows, selections and previews after %s and recovers failed reads", async kind => {
  prepare();
  const stored = storage();
  let revoked = 0;
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() { revoked++; } });
  let hidden = state(true);
  let failed = false;
  let denied = false;
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/lock")) { hidden = state(false); return Response.json({}); }
    if (url.endsWith("/trash")) { hidden = state(false); return problem(403, "Hidden locked"); }
    if (url.endsWith("/photos/hidden")) return failed ? problem(503, "Access unavailable") : Response.json(hidden);
    if (url.includes("/assets/query")) return denied ? problem(403, "Hidden locked") : Response.json({ items: [{ ...photo(1), previews: { ...photo(1).previews, grid: { state: "ready", generation_id: "synthetic" } } }], total: 1 });
    return new Response("synthetic-jpeg", { headers: { "Cache-Control": "no-store" } });
  });
  vi.stubGlobal("fetch", fetcher);
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await waitFor(() => expect(screen.getByAltText("Photo 1.jpg")).toBeTruthy());
  const cacheName = stored.open.mock.calls[0][0];
  const grid = screen.getByRole("main", { name: "Photo library" });
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/photos/hidden"))).toHaveLength(2));
  expect(screen.getByRole("main", { name: "Photo library" })).toBe(grid);
  expect(screen.getByText("1 selected photo")).toBeTruthy();
  expect(fetcher.mock.calls.filter(([url]) => url.includes("/assets/query"))).toHaveLength(1);
  if (kind === "lock") await fireEvent.click(screen.getByRole("button", { name: "Lock" }));
  else if (kind === "trash denial") { await fireEvent.click(screen.getByRole("button", { name: "Move to trash" })); await fireEvent.click(screen.getAllByRole("button", { name: "Move to trash" }).at(-1)!); }
  else if (kind === "denial") { denied = true; hidden = state(false); await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" })); }
  else if (kind === "expire") { vi.spyOn(Date, "now").mockReturnValue(Date.now() + 300_001); }
  else {
    if (kind === "revoke") hidden = state(false); else failed = true;
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
  }
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  expect(revoked).toBeGreaterThan(0);
  if (kind === "read") {
    await screen.findByText("Access unavailable");
    failed = false;
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    expect(screen.queryByText("Access unavailable")).toBeNull();
    expect(screen.queryByText("1 selected photo")).toBeNull();
  }
});

it("failed access reads preserve an in-flight Lock and its action error", async () => {
  prepare();
  let settle!: (response: Response) => void;
  let actionSignal: AbortSignal | undefined;
  let failing = false;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/lock")) { actionSignal = options?.signal ?? undefined; return new Promise<Response>(resolve => settle = resolve); }
    if (url.endsWith("/photos/hidden")) return failing ? problem(503, "Access unavailable") : Response.json(state(true));
    return Response.json({ items: [photo(1)], total: 1 });
  }));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Lock" }));
  failing = true;
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Access unavailable");
  expect(actionSignal?.aborted).toBe(false);
  settle(problem(503, "Lock unavailable"));
  await screen.findByText("Lock unavailable");
  failing = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("main", { name: "Photo library" });
  expect(screen.getByText("Lock unavailable")).toBeTruthy();
});

it.each(["read", "action"])("routes Hidden %s HTTP 401 to the session owner", async source => {
  const expired = vi.fn();
  vi.stubGlobal("fetch", vi.fn(async (url: string) => url.endsWith("/unlock") || source === "read" ? problem(401, "Session expired") : Response.json(state(false))));
  render(HiddenPhotos, { session: "synthetic", onauthfailure: expired });
  if (source === "action") {
    await waitFor(() => expect((screen.getByLabelText("Passcode", { exact: true }) as HTMLInputElement).disabled).toBe(false));
    await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "synthetic" } });
    await fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  }
  await waitFor(() => expect(expired).toHaveBeenCalledOnce());
});
