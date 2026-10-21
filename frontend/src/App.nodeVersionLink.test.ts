import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { storage, photo, photoMetadataResponse } from "./photo-test-fixtures.js";
import { node as fileNode, version } from "./version-test-fixtures.js";
import type { Node } from "./generated/docbank.js";

const oldID = "11111111-1111-4111-8111-111111111111";
const currentID = "22222222-2222-4222-8222-222222222222";
const root: Node = {
  id: 1,
  name: "",
  kind: "dir",
  path: "/",
  size: 0,
  revision: 1,
  created_at: "2026-01-01T00:00:00Z",
  modified_at: "2026-01-01T00:00:00Z",
};
const node: Node = {
  ...fileNode,
  name: "example.txt",
  kind: "file",
  path: "/reports/example.txt",
  current_version_id: currentID,
  blob_hash: "b".repeat(64),
  size: 200,
  revision: 1001,
};
const oldVersion = { ...version(oldID, 1, "content_create", "a"), size: 7 };
const currentVersion = { ...version(currentID, 1001, "content_replace", "b"), size: 200 };
const link = (id = oldID) => `#node=42&version=${id}`;
const credentials = "web_session=synthetic&web_upload_secret=proof&";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => {
  cleanup();
  Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
  history.replaceState(null, "", "/");
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
});

function api(
  override?: (url: string, init?: RequestInit) => Response | Promise<Response> | undefined,
) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    const overridden = override?.(url, init);
    if (overridden) return overridden;
    if (url === "/api/daemon/web-auth") return Response.json({ enabled: true });
    if (url === "/api/daemon/web-auth/login")
      return Response.json({
        enabled: true,
        session: { token: "signed-in", upload_secret: "proof" },
      });
    if (url === "/api/v1/path?path=%2F") return Response.json(root);
    if (url.includes("/children"))
      return Response.json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/42") return Response.json(node);
    if (url === `/api/v1/versions/${oldID}`) return Response.json(oldVersion);
    if (url === `/api/v1/versions/${currentID}`) return Response.json(currentVersion);
    if (url === "/api/v1/nodes/42/versions?limit=1000&offset=0")
      return Response.json({
        items: [
          currentVersion,
          ...Array.from({ length: 999 }, (_, index) =>
            version(
              `${(index + 1).toString(16).padStart(8, "0")}-3333-4333-8333-333333333333`,
              1000 - index,
              "content_replace",
              "b",
            ),
          ),
        ],
        total: 1001,
        limit: 1000,
        offset: 0,
      });
    if (url.includes("processing-profiles")) return Response.json([]);
    return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
}

async function signIn() {
  await fireEvent.input(await screen.findByLabelText("API key"), {
    target: { value: "synthetic-key" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

it("opens a node absent from directory rows and downloads its exact historical authority", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  const fetchMock = api((url) =>
    url === "/api/daemon/web-download"
      ? Response.json({ detail: "Download unavailable in fixture" }, { status: 503 })
      : undefined,
  );
  render(App);
  expect(await screen.findByText(oldID)).toBeTruthy();
  await screen.findByText("1001 retained versions");
  expect(screen.getByText("a".repeat(64))).toBeTruthy();
  expect(screen.getByText("7 B (7 bytes)")).toBeTruthy();
  expect(location.hash).toBe(link());
  await fireEvent.click(screen.getByRole("button", { name: "Download this version" }));
  await screen.findByText("Download unavailable in fixture");
  const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/daemon/web-download");
  expect(JSON.parse(String(call?.[1]?.body))).toEqual({
    node_id: 42,
    revision: 1001,
    version_id: oldID,
    blob_hash: "a".repeat(64),
    size: 7,
  });
  for (const [input, init] of fetchMock.mock.calls.filter(([input]) =>
    ["/api/v1/nodes/42", `/api/v1/versions/${oldID}`].includes(String(input)),
  )) {
    expect(new Headers(init?.headers).get("X-Docbank-Web-Session"), String(input)).toBe(
      "synthetic",
    );
  }
});

it.each(["missing node", "purged version", "wrong node", "bad request"])(
  "shows unavailable for %s without current substitution",
  async (kind) => {
    history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
    api((url) => {
      if (kind === "bad request" && url === "/api/v1/nodes/42")
        return Response.json({ detail: "Request rejected by the server" }, { status: 400 });
      if (kind === "missing node" && url === "/api/v1/nodes/42")
        return Response.json({ detail: "missing" }, { status: 404 });
      if (url === `/api/v1/versions/${oldID}`)
        return kind === "wrong node"
          ? Response.json({ ...oldVersion, node_id: 99 })
          : kind === "purged version"
            ? Response.json({ detail: "missing" }, { status: 404 })
            : undefined;
      return undefined;
    });
    render(App);
    expect(
      await screen.findByText(
        kind === "wrong node"
          ? /version does not belong to/
          : kind === "bad request"
            ? "Request rejected by the server"
            : /node or version is missing or purged/,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    expect(screen.queryByText(currentID)).toBeNull();
  },
);

it.each(["resolved", "invalid"])(
  "keeps the %s linked selection when root rows arrive after sign-in",
  async (kind) => {
    history.replaceState(null, "", kind === "resolved" ? `/${link()}` : "/#node=0&version=invalid");
    let release!: (response: Response) => void;
    api((url) =>
      url.includes("/children")
        ? new Promise<Response>((resolve) => {
            release = resolve;
          })
        : undefined,
    );
    render(App);
    await signIn();
    await screen.findByText(kind === "resolved" ? oldID : /Invalid node\/version link/);
    await waitFor(() => expect(release).toBeTypeOf("function"));
    await act(() =>
      release(
        Response.json({
          directory: root,
          items: [{ ...node, id: 99, name: "other.txt" }],
          total: 1,
          limit: 1000,
          offset: 0,
        }),
      ),
    );
    await screen.findByRole("cell", { name: "other.txt" });
    expect(
      screen.getByText(kind === "resolved" ? oldID : /Invalid node\/version link/),
    ).toBeTruthy();
    expect(location.hash).toBe(kind === "resolved" ? link() : "#node=0&version=invalid");
  },
);

it.each([`/api/v1/versions/${oldID}`, "/api/v1/nodes/42/versions?limit=1000&offset=0"])(
  "preserves linked intent through sign-in when %s rejects the session",
  async (failingRequest) => {
    history.replaceState(null, "", `/${link()}&query=%7B%7D`);
    let expired = true;
    api((url) => {
      if (url === failingRequest && expired) {
        expired = false;
        return Response.json({ detail: "expired" }, { status: 401 });
      }
      return undefined;
    });
    render(App);
    await signIn();
    await screen.findByText(/browser session expired or was rejected/);
    expect(location.hash).toBe(link());
    await signIn();
    expect(await screen.findByText(oldID)).toBeTruthy();
  },
);

it("discards late A when hash navigation selects B and handles back/forward events", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  let release!: (response: Response) => void;
  let oldSignal: AbortSignal | null | undefined;
  const fetchMock = api((url, init) => {
    if (url === `/api/v1/versions/${oldID}` && !release) {
      oldSignal = init?.signal;
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }
    return undefined;
  });
  render(App);
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await act(() => {
    history.pushState(null, "", `/${link(currentID)}`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await screen.findByText(currentID);
  expect(oldSignal?.aborted).toBe(true);
  await act(() => release(Response.json(oldVersion)));
  expect(screen.queryByText(oldID)).toBeNull();
  await act(() => {
    history.replaceState(null, "", `/${link()}`);
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await screen.findByText(oldID);
  await act(() => {
    history.replaceState(null, "", "/#photo=example");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  expect(screen.queryByText(oldID)).toBeNull();
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input) === `/api/v1/versions/${currentID}`),
  ).toHaveLength(1);
});

it("reopens exact versions on browser back and forward in the mounted drawer", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  let pending = false;
  let release!: (response: Response) => void;
  const fetchMock = api((url) => {
    if (pending && url === "/api/v1/nodes/42")
      return Response.json({ ...node, trashed_at: "2026-10-01T00:00:00Z" });
    if (pending && url === `/api/v1/versions/${oldID}`)
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    return undefined;
  });
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("1001 retained versions");
  const drawer = screen.getByRole("dialog");
  fetchMock.mockClear();
  await fireEvent.click(screen.getByText("Revision 1001").closest("button")!);
  await screen.findByText(currentID);
  expect(location.hash).toBe(link(currentID));
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls).toHaveLength(0);
  const push = vi.spyOn(history, "pushState");
  await fireEvent.click(
    within(screen.getByRole("region", { name: "Retained versions" }))
      .getByText("Revision 1001")
      .closest("button")!,
  );
  expect(push).not.toHaveBeenCalled();
  pending = true;
  history.back();
  await waitFor(() => expect(release).toBeTypeOf("function"));
  expect(location.hash).toBe(link());
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(screen.queryByText(currentID)).toBeNull();
  expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
  await act(() => release(Response.json(oldVersion)));
  await screen.findByText(oldID);
  expect(screen.getByText("Trash", { selector: "span" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
    "/api/v1/nodes/42",
    `/api/v1/versions/${oldID}`,
  ]);
  pending = false;
  history.forward();
  await screen.findByText(currentID);
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
    "/api/v1/nodes/42",
    `/api/v1/versions/${oldID}`,
    "/api/v1/nodes/42",
    `/api/v1/versions/${currentID}`,
  ]);
  expect(location.hash).toBe(link(currentID));
});

it("puts ordinary drawer selections in history and preserves other hash keys", async () => {
  history.replaceState(null, "", `/#${credentials}source=example`);
  api((url) => {
    if (url.includes("/children"))
      return Response.json({ directory: root, items: [node], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/42/versions?limit=1000&offset=0")
      return Response.json({
        items: [currentVersion, oldVersion],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    return undefined;
  });
  const push = vi.spyOn(history, "pushState");
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "example.txt" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Version history" }));
  await within(await screen.findByRole("dialog")).findByText(currentID);
  expect(location.hash).toBe(`#source=example&node=42&version=${currentID}`);
  expect(push).not.toHaveBeenCalled();
  const drawer = screen.getByRole("dialog");
  await fireEvent.click(screen.getByText("Revision 1").closest("button")!);
  await screen.findByText(oldID);
  expect(location.hash).toBe(`#source=example&node=42&version=${oldID}`);
  history.back();
  await within(drawer).findByText(currentID);
  expect(location.hash).toBe(`#source=example&node=42&version=${currentID}`);
  expect(screen.getByRole("dialog")).toBe(drawer);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  expect(location.hash).toBe("#source=example");
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("closes row-opened version history when a pending tag removal selects the next file", async () => {
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  history.replaceState(null, "", `/#${credentials}`);
  const vaultID = "55555555-5555-4555-8555-555555555555";
  const tag = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 2,
  };
  localStorage.setItem(
    `docbank:tag-hotkeys:v1:${vaultID}`,
    JSON.stringify({ version: 1, bindings: { "1": tag.id } }),
  );
  const otherNode = { ...node, id: 99, name: "other.txt", path: "/other.txt" };
  let removed = false;
  let release!: (response: Response) => void;
  api((url, init) => {
    if (url.includes("/children"))
      return Response.json({
        directory: root,
        items: [node, otherNode],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    if (url.startsWith("/api/v1/audit/status"))
      return Response.json({
        enabled: false,
        vault_id: vaultID,
        operation_sequence_high_water: 0,
        allocation_entry_count: 0,
        scopes: [],
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return Response.json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === `/api/v1/tags/${tag.id}/nodes?limit=1000&offset=0&live_only=true`) {
      const nodes = removed ? [otherNode] : [node, otherNode];
      return Response.json({
        items: nodes.map((node) => ({ node, path: node.path })),
        total: nodes.length,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/nodes/42/tags?limit=1000&offset=0")
      return Response.json({
        items: removed ? [] : [tag],
        total: removed ? 0 : 1,
        limit: 1000,
        offset: 0,
      });
    if (url === `/api/v1/nodes/42/tags/${tag.id}` && init?.method === "DELETE")
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    if (url === "/api/v1/nodes/99") return Response.json(otherNode);
    return undefined;
  });
  render(App);
  await screen.findByRole("cell", { name: "example.txt" });
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(await screen.findByRole("option", { name: "tax (2)" }));
  await fireEvent.click(await screen.findByRole("cell", { name: "/reports/example.txt" }));
  expect((await screen.findByRole("group", { name: "Assigned tags" })).textContent).toContain(
    "tax",
  );
  await fireEvent.keyDown(window, { key: "1" });
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await fireEvent.click(screen.getByRole("button", { name: "Version history" }));
  await within(await screen.findByRole("dialog")).findByText(currentID);
  expect(location.hash).toBe(link(currentID));

  await act(() => {
    removed = true;
    release(
      Response.json({
        tag: { ...tag, revision: 2, assignment_count: 1 },
        node: { ...node, revision: 1002 },
        changed: true,
      }),
    );
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.queryByRole("cell", { name: "/reports/example.txt" })).toBeNull();
  expect(screen.getByLabelText("Document authority for other.txt")).toBeTruthy();
});

it("replaces an ordinary version drawer when a linked version opens in the same tab", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  api((url) =>
    url.includes("/children")
      ? Response.json({ directory: root, items: [node], total: 1, limit: 1000, offset: 0 })
      : undefined,
  );
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "example.txt" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Version history" }));
  await within(await screen.findByRole("dialog")).findByText(currentID);
  await act(() => {
    location.hash = link();
  });
  await screen.findByText(oldID);
  expect(screen.getAllByRole("dialog")).toHaveLength(1);
  expect(within(screen.getByRole("dialog")).getByText(oldID)).toBeTruthy();
});

it("keeps the linked drawer when URL navigation adds or removes a query", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  const fetchMock = api();
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("1001 retained versions");
  const drawer = screen.getByRole("dialog");
  fetchMock.mockClear();
  await act(() => {
    location.hash = `${link()}&query=%7B%7D`;
  });
  await waitFor(() => expect(location.hash).toContain("query="));
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(screen.getByText(oldID)).toBeTruthy();
  await act(() => {
    location.hash = link();
  });
  await waitFor(() => expect(location.hash).toBe(link()));
  expect(screen.getByText(oldID)).toBeTruthy();
  expect(fetchMock.mock.calls).toHaveLength(0);
});

it("keeps a pending resolve when URL navigation edits the query", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  let release!: (response: Response) => void;
  let signal: AbortSignal | null | undefined;
  const fetchMock = api((url, init) => {
    if (url === `/api/v1/versions/${oldID}`) {
      signal = init?.signal;
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }
    return undefined;
  });
  render(App);
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await act(() => {
    history.replaceState(history.state, "", `/${link()}&query=%7B%7D`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  expect(signal?.aborted).toBe(false);
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input) === "/api/v1/nodes/42"),
  ).toHaveLength(1);
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input) === `/api/v1/versions/${oldID}`),
  ).toHaveLength(1);
  await act(() => release(Response.json(oldVersion)));
  expect(await screen.findByText(oldID)).toBeTruthy();
  expect(location.hash).toBe(`${link()}&query=%7B%7D`);
});

it.each(["browser session", "API key session"])(
  "a download 401 clears the query and preserves the version link for a %s",
  async (session) => {
    history.replaceState(
      null,
      "",
      `/#${session === "browser session" ? credentials : ""}${link().slice(1)}&query=%7B%7D`,
    );
    api((url) =>
      url === "/api/daemon/web-download"
        ? Response.json({ detail: "expired" }, { status: 401 })
        : undefined,
    );
    render(App);
    if (session === "API key session") await signIn();
    await screen.findByText(oldID);
    await fireEvent.click(screen.getByRole("button", { name: "Download this version" }));
    await screen.findByText("Open your Docbank");
    expect(location.hash).toBe(link());
    expect(screen.queryByRole("dialog")).toBeNull();
  },
);

it("keeps the folder row path after auto-selecting a version without a node path", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  api((url) => {
    if (url === "/api/v1/nodes/42") return Response.json({ ...node, path: undefined });
    if (url.includes("/children"))
      return Response.json({
        directory: { ...root, path: "/Reports" },
        items: [{ ...node, path: undefined }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    return undefined;
  });
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "example.txt" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Version history" }));
  await screen.findByText(currentID);
  await act(() => {});
  expect(
    screen.getByRole("dialog", { name: "Immutable version history for /Reports/example.txt" }),
  ).toBeTruthy();
});

it("keeps a linked version when photo cleanup rewrites a combined fragment", async () => {
  history.replaceState(null, "", `/photos/hidden#${credentials}${link().slice(1)}&photo=example`);
  storage();
  api((url) => (url.endsWith("/photos/hidden") ? Response.json({ configured: true }) : undefined));
  render(App);
  await screen.findByText(oldID);
  expect(new URLSearchParams(location.hash.slice(1)).get("photo")).toBe("example");
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(location.hash).toBe(link()));
  expect(await screen.findByText(oldID)).toBeTruthy();
});

it("returns focus to the photo viewer after closing linked history", async () => {
  history.replaceState(null, "", `/photos#${credentials}photo=photo-1`);
  storage();
  api(
    (url, init) =>
      photoMetadataResponse(url, init) ??
      (url.endsWith("/photos/hidden")
        ? Response.json({ configured: false })
        : url.endsWith("/photos/assets/query")
          ? Response.json({ items: [photo(1)], total: 1 })
          : undefined),
  );
  render(App);
  const viewer = await screen.findByRole("dialog", { name: /Photo viewer/ });
  screen.getByRole("button", { name: "Close photo viewer" }).focus();
  await act(() => {
    history.pushState(null, "", `/photos${link()}&photo=photo-1`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await screen.findByText(oldID);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  await waitFor(() => expect(document.activeElement).toBe(viewer));
});

it("discards a delayed linked version after closing it and opening another versions drawer", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  const otherNode = { ...node, id: 99, name: "other.txt", path: "/other.txt" };
  const otherVersion = { ...currentVersion, node_id: 99 };
  let release!: (response: Response) => void;
  let releaseHistory!: (response: Response) => void;
  api((url) => {
    if (url.includes("/children"))
      return Response.json({
        directory: root,
        items: [otherNode],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/nodes/99") return Response.json(otherNode);
    if (url === `/api/v1/versions/${oldID}`)
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    if (url === "/api/v1/nodes/99/versions?limit=1000&offset=0")
      return new Promise<Response>((resolve) => {
        releaseHistory = resolve;
      });
    return undefined;
  });
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "other.txt" }));
  await act(() => {
    history.pushState(null, "", `/${link()}`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Linked content version" })).getByRole("button", {
      name: "Close",
    }),
  );
  expect(location.hash).toBe("");
  await fireEvent.click(screen.getByRole("button", { name: "Version history" }));
  await waitFor(() => expect(releaseHistory).toBeTypeOf("function"));
  await act(() => release(Response.json(oldVersion)));
  expect(screen.queryByText(oldID)).toBeNull();
  await act(() =>
    releaseHistory(Response.json({ items: [otherVersion], total: 1, limit: 1000, offset: 0 })),
  );
  await within(screen.getByRole("dialog")).findByText(currentID);
  expect(
    screen.getByRole("dialog", { name: "Immutable version history for /other.txt" }),
  ).toBeTruthy();
  expect(location.hash).toBe(`#node=99&version=${currentID}`);
});

it("returns to the grid after selecting a version over a photo opened from the grid", async () => {
  history.replaceState(null, "", `/photos#${credentials}`);
  storage();
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  api(
    (url, init) =>
      photoMetadataResponse(url, init) ??
      (url.endsWith("/photos/hidden")
        ? Response.json({ configured: false })
        : url.endsWith("/photos/assets/query")
          ? Response.json({ items: [photo(1)], total: 1 })
          : url === "/api/v1/nodes/42/versions?limit=1000&offset=0"
            ? Response.json({
                items: [currentVersion, oldVersion],
                total: 2,
                limit: 1000,
                offset: 0,
              })
            : undefined),
  );
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await screen.findByRole("dialog", { name: /Photo viewer/ });
  expect(history.state?.photoReturn).toBeTruthy();
  await act(() => {
    history.replaceState(history.state, "", `/photos#photo=photo-1&${link().slice(1)}`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await screen.findByText(oldID);
  await fireEvent.click((await screen.findByText("Revision 1001")).closest("button")!);
  await screen.findByText(currentID);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  await fireEvent.click(screen.getByRole("button", { name: "Close photo viewer" }));
  await waitFor(() => expect(location.hash).toBe(""));
  expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Close version history" })).toBeNull();
  expect(screen.getByRole("button", { name: "Open Photo 1.jpg" })).toBeTruthy();
});
