import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";

const store = { getItem: vi.fn(() => null), setItem: vi.fn(), removeItem: vi.fn() };
beforeEach(() => {
  store.setItem.mockClear();
  vi.stubGlobal("localStorage", store);
  vi.stubGlobal("sessionStorage", store);
});

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("offers the API key and clears it after rejected sign-in", async () => {
  history.replaceState(null, "", "/");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input) === "/api/daemon/web-auth")
      return new Response(JSON.stringify({ enabled: true }));
    return new Response(JSON.stringify({ detail: "invalid API key" }), { status: 401 });
  });
  render(App);
  const field = await screen.findByLabelText("API key");
  await fireEvent.input(field, { target: { value: "synthetic-browser-credential-0123456789" } });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByText("invalid API key");
  await waitFor(() => expect((field as HTMLInputElement).value).toBe(""));
  const call = fetchMock.mock.calls.find(([input]) => String(input).endsWith("/login"));
  expect(JSON.parse(String(call?.[1]?.body))).toEqual({
    api_key: "synthetic-browser-credential-0123456789",
  });
  expect(
    store.setItem.mock.calls.some((call) => String(call).includes("synthetic-browser-credential")),
  ).toBe(false);
});

it.each([false, true])(
  "signs out immediately with a final duration, failure=%s",
  async (failure) => {
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    let resolveRevoke!: (response: Response) => void;
    let rejectRevoke!: (cause: Error) => void;
    history.replaceState(null, "", "/");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = String(input);
      if (url === "/api/daemon/web-auth") return new Response(JSON.stringify({ enabled: true }));
      if (url.endsWith("/login"))
        return new Response(
          JSON.stringify({
            enabled: true,
            session: { token: "tab-token", upload_secret: "proof" },
          }),
        );
      if (url.startsWith("/api/v1/path"))
        return new Response(
          JSON.stringify({ id: 1, kind: "dir", name: "", path: "/", revision: 1 }),
        );
      if (url.includes("/children"))
        return new Response(
          JSON.stringify({
            directory: { id: 1, kind: "dir", path: "/", revision: 1 },
            items: [],
            total: 0,
          }),
        );
      if (url.startsWith("/api/daemon/web-session"))
        return new Promise<Response>((resolve, reject) => {
          resolveRevoke = resolve;
          rejectRevoke = reject;
        });
      return new Response(JSON.stringify({ items: [], total: 0 }));
    });
    render(App);
    const key = await screen.findByLabelText("API key");
    await fireEvent.input(key, { target: { value: "synthetic-master-key" } });
    await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    const signOut = await screen.findByRole("button", { name: "Sign out" });
    now = 120_000;
    await fireEvent.click(signOut);
    expect(screen.getByLabelText("API key")).toBeTruthy();
    const revokes = fetchMock.mock.calls.filter(
      ([input, init]) =>
        String(input).startsWith("/api/daemon/web-session") && init?.method === "DELETE",
    );
    expect(revokes).toHaveLength(1);
    expect(String(revokes[0]?.[0])).toBe("/api/daemon/web-session?duration_bucket=1_to_5m");
    expect(new Headers(revokes[0]?.[1]?.headers).get("X-Docbank-Web-Session")).toBe("tab-token");
    const reports = () =>
      fetchMock.mock.calls.filter(
        ([input, init]) =>
          String(input) === "/api/daemon/telemetry/events" &&
          JSON.parse(String(init?.body)).event === "session_ended",
      );
    expect(reports()).toHaveLength(0);
    if (failure) rejectRevoke(new Error("offline"));
    else resolveRevoke(new Response(null, { status: 204 }));
    cleanup();
    expect(reports()).toHaveLength(0);
  },
);

it("clears an open Manage tags dialog after session expiry before signing in again", async () => {
  history.replaceState(null, "", "/");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1,
    parent_id: null,
    name: "",
    kind: "dir",
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/",
  };
  const file = {
    id: 3,
    parent_id: 1,
    name: "readme.txt",
    kind: "file",
    size: 10,
    mime_type: "text/plain",
    current_version_id: "33333333-1111-4111-8111-111111111111",
    blob_hash: "3".repeat(64),
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/readme.txt",
  };
  const tax = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 1,
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  let logins = 0;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/daemon/web-auth") return json({ enabled: true });
    if (url.endsWith("/login")) {
      logins += 1;
      return json({
        enabled: true,
        session: { token: `tab-token-${logins}`, upload_secret: `proof-${logins}` },
      });
    }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return json(file);
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE")
      return json({ detail: "session expired" }, 401);
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
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain(
    "/api/v1/nodes/3/tags?limit=1000&offset=0",
  );
  expect(dialog.textContent).toContain("tax");
  await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
  await screen.findByLabelText("API key");
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Manage tags for readme.txt" })).toBeNull(),
  );

  const reLoginKey = screen.getByLabelText("API key");
  await fireEvent.input(reLoginKey, { target: { value: "synthetic-master-key" } });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("button", { name: "Sign out" });
  expect(screen.queryByRole("dialog", { name: "Manage tags for readme.txt" })).toBeNull();
  expect(logins).toBe(2);
  expect(
    fetchMock.mock.calls.some(
      ([input, init]) =>
        String(input) === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE",
    ),
  ).toBe(true);
});

it("keeps a new sign-in after an old tag request returns a delayed 401", async () => {
  history.replaceState(null, "", "/");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1,
    parent_id: null,
    name: "",
    kind: "dir",
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/",
  };
  const file = {
    id: 3,
    parent_id: 1,
    name: "readme.txt",
    kind: "file",
    size: 10,
    mime_type: "text/plain",
    current_version_id: "33333333-1111-4111-8111-111111111111",
    blob_hash: "3".repeat(64),
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/readme.txt",
  };
  const tax = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 1,
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  let resolveDelete!: (response: Response) => void;
  let markDeleteStarted!: () => void;
  const delayedDelete = new Promise<Response>((resolve) => {
    resolveDelete = resolve;
  });
  const deleteStarted = new Promise<void>((resolve) => {
    markDeleteStarted = resolve;
  });
  let logins = 0;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/daemon/web-auth") return json({ enabled: true });
    if (url.endsWith("/login")) {
      logins += 1;
      return json({
        enabled: true,
        session: { token: `tab-token-${logins}`, upload_secret: `proof-${logins}` },
      });
    }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return json(file);
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
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
  expect(
    fetchMock.mock.calls.some(
      ([input, init]) =>
        String(input) === `/api/v1/nodes/3/tags/${tax.id}` && init?.method === "DELETE",
    ),
  ).toBe(true);
});

it("ignores a delayed trash success after signing in again", async () => {
  history.replaceState(null, "", "/");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1,
    parent_id: null,
    name: "",
    kind: "dir",
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/",
  };
  const file = {
    id: 3,
    parent_id: 1,
    name: "readme.txt",
    kind: "file",
    size: 10,
    mime_type: "text/plain",
    current_version_id: "33333333-1111-4111-8111-111111111111",
    blob_hash: "3".repeat(64),
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/readme.txt",
  };
  const tax = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 1,
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  let resolveTrash!: (response: Response) => void;
  let markTrashStarted!: () => void;
  const delayedTrash = new Promise<Response>((resolve) => {
    resolveTrash = resolve;
  });
  const trashStarted = new Promise<void>((resolve) => {
    markTrashStarted = resolve;
  });
  let logins = 0;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/daemon/web-auth") return json({ enabled: true });
    if (url.endsWith("/login")) {
      logins += 1;
      return json({
        enabled: true,
        session: { token: `tab-token-${logins}`, upload_secret: `proof-${logins}` },
      });
    }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return json(file);
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3/trash") {
      markTrashStarted();
      return delayedTrash;
    }
    if (url === "/api/daemon/web-session") return new Response(null, { status: 204 });
    if (url.includes("processing-profiles")) return json([]);
    return json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  const rootLoads = () =>
    fetchMock.mock.calls.filter(([input]) => String(input) === "/api/v1/path?path=%2F").length;

  render(App);
  await fireEvent.input(await screen.findByLabelText("API key"), {
    target: { value: "synthetic-master-key" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("button", { name: "Sign out" });
  await fireEvent.click(await screen.findByRole("cell", { name: "readme.txt" }));
  await screen.findByText("1 assigned");
  await fireEvent.click(screen.getAllByRole("button", { name: /Move to trash/ })[0]!);
  const dialog = await screen.findByRole("dialog", { name: "Move readme.txt to trash" });
  await fireEvent.click(within(dialog).getByRole("button", { name: /Move to trash/ }));
  await trashStarted;

  await fireEvent.click(
    document.querySelector<HTMLButtonElement>('button[aria-label="Sign out"]')!,
  );
  await fireEvent.input(await screen.findByLabelText("API key"), {
    target: { value: "synthetic-master-key" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("button", { name: "Sign out" });
  await fireEvent.click(await screen.findByRole("cell", { name: "readme.txt" }));
  await screen.findByText("1 assigned");
  const loadsBeforeReply = rootLoads();

  resolveTrash(json({ ...file, revision: 2, trashed_at: "2026-09-10T00:00:00Z" }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(screen.getByText("1 assigned")).toBeTruthy();
  expect(rootLoads()).toBe(loadsBeforeReply);
  expect(logins).toBe(2);
});

function delayedReplyFixture(isDelayed: (url: string, init?: RequestInit) => boolean) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1,
    parent_id: null,
    name: "",
    kind: "dir",
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/",
  };
  const file = {
    id: 3,
    parent_id: 1,
    name: "readme.txt",
    kind: "file",
    size: 10,
    mime_type: "text/plain",
    current_version_id: "33333333-1111-4111-8111-111111111111",
    blob_hash: "3".repeat(64),
    revision: 1,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: "/readme.txt",
  };
  const trashed = {
    id: 4,
    parent_id: 1,
    name: "old.txt",
    kind: "file",
    size: 5,
    mime_type: "text/plain",
    current_version_id: "44444444-1111-4111-8111-111111111111",
    blob_hash: "4".repeat(64),
    revision: 2,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    trashed_at: "2026-09-10T00:00:00Z",
    path: "/old.txt",
  };
  const tax = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 1,
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  let resolveDelayed!: (response: Response) => void;
  let markStarted!: () => void;
  const delayed = new Promise<Response>((resolve) => {
    resolveDelayed = resolve;
  });
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  const state = { logins: 0 };
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (isDelayed(url, init)) {
      markStarted();
      return delayed;
    }
    if (url === "/api/daemon/web-auth") return json({ enabled: true });
    if (url.endsWith("/login")) {
      state.logins += 1;
      return json({
        enabled: true,
        session: { token: `tab-token-${state.logins}`, upload_secret: `proof-${state.logins}` },
      });
    }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({ directory: root, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return json(file);
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0")
      return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/trash?limit=1000&offset=0")
      return json({ items: [trashed], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/daemon/web-session") return new Response(null, { status: 204 });
    if (url.includes("processing-profiles")) return json([]);
    return json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  const signIn = async () => {
    await fireEvent.input(await screen.findByLabelText("API key"), {
      target: { value: "synthetic-master-key" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("button", { name: "Sign out" });
  };
  const signInAgainAndSelect = async () => {
    await fireEvent.click(
      document.querySelector<HTMLButtonElement>('button[aria-label="Sign out"]')!,
    );
    await signIn();
    await fireEvent.click(await screen.findByRole("cell", { name: "readme.txt" }));
    await screen.findByText("1 assigned");
  };
  const rootLoads = () =>
    fetchMock.mock.calls.filter(([input]) => String(input) === "/api/v1/path?path=%2F").length;
  return {
    json,
    file,
    trashed,
    tax,
    state,
    started,
    resolveDelayed,
    signIn,
    signInAgainAndSelect,
    rootLoads,
  };
}

it("ignores a delayed restore success after signing in again", async () => {
  history.replaceState(null, "", "/");
  const fixture = delayedReplyFixture((url) => url === "/api/v1/nodes/4/restore");
  render(App);
  await fixture.signIn();
  await fireEvent.click(screen.getAllByRole("button", { name: "Recoverable trash" })[0]!);
  await fireEvent.click(await screen.findByRole("button", { name: /^Restore$/ }));
  const dialog = await screen.findByRole("dialog", { name: /Restore old\.txt/ });
  await fireEvent.click(within(dialog).getByRole("button", { name: /Restore/ }));
  await fixture.started;
  await fixture.signInAgainAndSelect();
  const loadsBeforeReply = fixture.rootLoads();

  fixture.resolveDelayed(fixture.json({ ...fixture.trashed, revision: 3, trashed_at: null }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(screen.getByText("1 assigned")).toBeTruthy();
  expect(fixture.rootLoads()).toBe(loadsBeforeReply);
  expect(fixture.state.logins).toBe(2);
});

it("ignores a delayed tag rename success after signing in again", async () => {
  history.replaceState(null, "", "/");
  const fixture = delayedReplyFixture(
    (url, init) =>
      url === `/api/v1/tags/${"33333333-3333-4333-8333-333333333333"}` && init?.method === "PATCH",
  );
  render(App);
  await fixture.signIn();
  const manage = await screen.findByRole("button", { name: "Manage tag definitions" });
  await waitFor(() => expect((manage as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(manage);
  const dialog = await screen.findByRole("dialog", { name: "Manage tag definitions" });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename tag tax" }));
  await fireEvent.input(within(dialog).getByLabelText("Rename tax"), {
    target: { value: "renamed-tax" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Save renamed tag tax" }));
  await fixture.started;
  await fixture.signInAgainAndSelect();

  fixture.resolveDelayed(fixture.json({ ...fixture.tax, name: "renamed-tax", revision: 2 }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(screen.queryByText("renamed-tax")).toBeNull();
  expect(fixture.state.logins).toBe(2);
});
