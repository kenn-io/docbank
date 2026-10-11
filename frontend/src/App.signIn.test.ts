import { beforeEach, afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import App from "./App.svelte";

const store = { getItem: vi.fn(() => null), setItem: vi.fn(), removeItem: vi.fn() };
beforeEach(() => {
 store.setItem.mockClear();
 vi.stubGlobal("localStorage", store);
 vi.stubGlobal("sessionStorage", store);
});

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("offers the API key and clears it after rejected sign-in", async () => {
 history.replaceState(null, "", "/");
 const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
  if (String(input) === "/api/daemon/web-auth") return new Response(JSON.stringify({ enabled: true }));
  return new Response(JSON.stringify({ detail: "invalid API key" }), { status: 401 });
 });
 render(App);
 const field = await screen.findByLabelText("API key");
 await fireEvent.input(field, { target: { value: "synthetic-browser-credential-0123456789" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 await screen.findByText("invalid API key");
 await waitFor(() => expect((field as HTMLInputElement).value).toBe(""));
 expect((field as HTMLInputElement).value).toBe("");
 const call = fetchMock.mock.calls.find(([input]) => String(input).endsWith("/login"));
 expect(JSON.parse(String(call?.[1]?.body))).toEqual({ api_key: "synthetic-browser-credential-0123456789" });
 expect(store.setItem.mock.calls.some((call) => String(call).includes("synthetic-browser-credential"))).toBe(false);
});

it("exchanges the key before reading the vault and offers sign out", async () => {
 history.replaceState(null, "", "/");
 vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
 const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
  const url = String(input);
  if (url === "/api/daemon/web-auth") return new Response(JSON.stringify({ enabled: true }));
  if (url.endsWith("/login")) return new Response(JSON.stringify({ enabled: true, session: { token: "tab-token", upload_secret: "proof" } }));
  if (url.startsWith("/api/v1/path")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", path: "/", revision: 1 }));
  if (url.includes("/children")) return new Response(JSON.stringify({ directory: { id: 1, kind: "dir", path: "/", revision: 1 }, items: [], total: 0 }));
  if (url === "/api/daemon/web-session") return new Response(null, { status: 204 });
  return new Response(JSON.stringify({ items: [], total: 0 }));
 });
 render(App);
 const key = await screen.findByLabelText("API key");
 await fireEvent.input(key, { target: { value: "synthetic-master-key" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 const signOut = await screen.findByRole("button", { name: "Sign out" });
 expect((key as HTMLInputElement).value).toBe("");
 await fireEvent.click(signOut);
 await waitFor(() => expect(screen.getByLabelText("API key")).toBeTruthy());
 await waitFor(() => expect(fetchMock.mock.calls.some(([input]) => String(input) === "/api/daemon/web-session")).toBe(true), { timeout: 10_000 });
 const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/daemon/web-session");
 expect(new Headers(call?.[1]?.headers).get("X-Docbank-Web-Session")).toBe("tab-token");
 expect(call?.[1]?.redirect).toBe("error");
 expect(call?.[1]?.credentials).toBe("same-origin");
 vi.unstubAllGlobals();
});

it("clears an open Manage tags dialog after session expiry before signing in again", async () => {
 history.replaceState(null, "", "/");
 vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
 const root = { id: 1, parent_id: null, name: "", kind: "dir", revision: 1, created_at: "2026-09-09T00:00:00Z", modified_at: "2026-09-09T00:00:00Z", path: "/" };
 const file = { id: 3, parent_id: 1, name: "readme.txt", kind: "file", size: 10, mime_type: "text/plain", current_version_id: "33333333-1111-4111-8111-111111111111", blob_hash: "3".repeat(64), revision: 1, created_at: "2026-09-09T00:00:00Z", modified_at: "2026-09-09T00:00:00Z", path: "/readme.txt" };
 const tax = { id: "33333333-3333-4333-8333-333333333333", name: "tax", revision: 1, assignment_count: 1 };
 const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
 let logins = 0;
 const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
  const url = String(input);
  if (url === "/api/daemon/web-auth") return json({ enabled: true });
  if (url.endsWith("/login")) {
   logins += 1;
   return json({ enabled: true, session: { token: `tab-token-${logins}`, upload_secret: `proof-${logins}` } });
  }
  if (url === "/api/v1/path?path=%2F") return json(root);
  if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
  if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
  if (url === "/api/v1/nodes/3") return json(file);
  if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0") return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
  if (url === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE") return json({ detail: "session expired" }, 401);
  if (url === "/api/daemon/web-session") return new Response(null, { status: 204 });
  if (url.includes("processing-profiles")) return json([]);
  return json({ items: [], total: 0, limit: 1000, offset: 0 });
 });

 render(App);
 const key = await screen.findByLabelText("API key");
 await fireEvent.input(key, { target: { value: "synthetic-master-key" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 await screen.findByRole("button", { name: "Sign out" });
 await fireEvent.click(await screen.findByRole("cell", { name: "readme.txt" }));
 await screen.findByText("1 assigned");
 await fireEvent.click(await screen.findByRole("button", { name: "Manage" }));
 const dialog = await screen.findByRole("dialog", { name: "Manage tags for readme.txt" });
 expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain("/api/v1/nodes/3/tags?limit=1000&offset=0");
 expect(dialog.textContent).toContain("tax");
 await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
 await screen.findByLabelText("API key");
 await waitFor(() => expect(screen.queryByRole("dialog", { name: "Manage tags for readme.txt" })).toBeNull());

 const reLoginKey = screen.getByLabelText("API key");
 await fireEvent.input(reLoginKey, { target: { value: "synthetic-master-key" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 await screen.findByRole("button", { name: "Sign out" });
 expect(screen.queryByRole("dialog", { name: "Manage tags for readme.txt" })).toBeNull();
 expect(logins).toBe(2);
 expect(fetchMock.mock.calls.some(([input, init]) => String(input) === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE")).toBe(true);
});

it("keeps a new sign-in after an old tag request returns a delayed 401", async () => {
 history.replaceState(null, "", "/");
 vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
 const root = { id: 1, parent_id: null, name: "", kind: "dir", revision: 1, created_at: "2026-09-09T00:00:00Z", modified_at: "2026-09-09T00:00:00Z", path: "/" };
 const file = { id: 3, parent_id: 1, name: "readme.txt", kind: "file", size: 10, mime_type: "text/plain", current_version_id: "33333333-1111-4111-8111-111111111111", blob_hash: "3".repeat(64), revision: 1, created_at: "2026-09-09T00:00:00Z", modified_at: "2026-09-09T00:00:00Z", path: "/readme.txt" };
 const tax = { id: "33333333-3333-4333-8333-333333333333", name: "tax", revision: 1, assignment_count: 1 };
 const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
 let resolveDelete!: (response: Response) => void;
 let markDeleteStarted!: () => void;
 const delayedDelete = new Promise<Response>((resolve) => { resolveDelete = resolve; });
 const deleteStarted = new Promise<void>((resolve) => { markDeleteStarted = resolve; });
 let logins = 0;
 const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
  const url = String(input);
  if (url === "/api/daemon/web-auth") return json({ enabled: true });
  if (url.endsWith("/login")) {
   logins += 1;
   return json({ enabled: true, session: { token: `tab-token-${logins}`, upload_secret: `proof-${logins}` } });
  }
  if (url === "/api/v1/path?path=%2F") return json(root);
  if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
  if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
  if (url === "/api/v1/nodes/3") return json(file);
  if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0") return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
  if (url === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE") {
   markDeleteStarted();
   return delayedDelete;
  }
  if (url === "/api/daemon/web-session") return new Response(null, { status: 204 });
  if (url.includes("processing-profiles")) return json([]);
  return json({ items: [], total: 0, limit: 1000, offset: 0 });
 });

 render(App);
 const key = await screen.findByLabelText("API key");
 await fireEvent.input(key, { target: { value: "synthetic-master-key" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 await screen.findByRole("button", { name: "Sign out" });
 await fireEvent.click(await screen.findByRole("cell", { name: "readme.txt" }));
 await screen.findByText("1 assigned");
 await fireEvent.click(await screen.findByRole("button", { name: "Manage" }));
 await screen.findByRole("dialog", { name: "Manage tags for readme.txt" });
 await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
 await deleteStarted;

 const signOut = document.querySelector<HTMLButtonElement>('button[aria-label="Sign out"]');
 expect(signOut).not.toBeNull();
 await fireEvent.click(signOut!);
 await screen.findByLabelText("API key");
 const reLoginKey = screen.getByLabelText("API key");
 await fireEvent.input(reLoginKey, { target: { value: "synthetic-master-key" } });
 await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
 await screen.findByRole("button", { name: "Sign out" });

 resolveDelete(json({ detail: "old session expired" }, 401));
 await new Promise((resolve) => setTimeout(resolve, 0));

 expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
 expect(screen.queryByLabelText("API key")).toBeNull();
 expect(logins).toBe(2);
 expect(fetchMock.mock.calls.some(([input, init]) => String(input) === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE")).toBe(true);
});
