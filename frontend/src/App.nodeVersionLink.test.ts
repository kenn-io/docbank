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
      return Response.json({ items: [currentVersion], total: 1001, limit: 1000, offset: 0 });
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

it("shows retained trash without restoring it", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  const fetchMock = api((url) =>
    url === "/api/v1/nodes/42"
      ? Response.json({ ...node, trashed_at: "2026-10-01T00:00:00Z" })
      : undefined,
  );
  render(App);
  await screen.findByText(oldID);
  expect(screen.getByText("Trash", { selector: "span" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
  expect(fetchMock.mock.calls.some(([input]) => String(input).includes("/restore"))).toBe(false);
});

it.each(["missing node", "purged version", "wrong node"])(
  "shows unavailable for %s without current substitution",
  async (kind) => {
    history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
    api((url) => {
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
          : /node or version is missing or purged/,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    expect(screen.queryByText(currentID)).toBeNull();
  },
);

it("reports a malformed version ID as missing", async () => {
  history.replaceState(null, "", `/#${credentials}node=42&version=invalid`);
  const fetchMock = api((url) =>
    url === "/api/v1/versions/invalid"
      ? Response.json({ detail: "not found" }, { status: 404 })
      : undefined,
  );
  render(App);
  expect(await screen.findByText(/node or version is missing or purged/)).toBeTruthy();
  expect(fetchMock.mock.calls.some(([input]) => String(input) === "/api/v1/versions/invalid")).toBe(
    true,
  );
});

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

it("preserves linked intent through sign-in and a rejected session", async () => {
  history.replaceState(null, "", `/${link()}`);
  let expired = true;
  api((url) => {
    if (url === `/api/v1/versions/${oldID}` && expired) {
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
});

it("preserves linked intent when the history list expires after direct resolution", async () => {
  history.replaceState(null, "", `/${link()}`);
  let expired = true;
  api((url) => {
    if (url.includes("/nodes/42/versions?") && expired) {
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
});

it("closing the linked drawer clears only the selection and preserves a query", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}&query=%7B%7D`);
  api();
  render(App);
  await screen.findByText(oldID);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  await waitFor(() => expect(screen.queryByText(oldID)).toBeNull());
  expect(location.hash).toBe("#query=%7B%7D");
});

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

it("opens a normal query fragment without resolving a node/version", async () => {
  history.replaceState(null, "", `/#${credentials}query=%7B%7D`);
  const fetchMock = api();
  render(App);
  expect(await screen.findByRole("region", { name: "Query editor" })).toBeTruthy();
  expect(location.hash).toContain("query=");
  expect(
    fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/versions/")),
  ).toBe(false);
});

it("resumes the same exact selection after remounting and signing in", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  const app = render(App);
  await screen.findByText(oldID);
  app.unmount();
  render(App);
  await signIn();
  expect(await screen.findByText(oldID)).toBeTruthy();
  expect(location.hash).toBe(link());
});

it("reopens exact versions on browser back and forward in the mounted drawer", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  const fetchMock = api();
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
  history.back();
  await screen.findByText(oldID);
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
    "/api/v1/nodes/42",
    `/api/v1/versions/${oldID}`,
  ]);
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

it("clears the linked drawer when query editing replaces the hash", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  render(App);
  await screen.findByText(oldID);
  await fireEvent.click(screen.getByRole("button", { name: "Edit query" }));
  await screen.findByRole("region", { name: "Query editor" });
  await waitFor(() => expect(screen.queryByText(oldID)).toBeNull());
  expect(location.hash).toContain("query=");
  expect(location.hash).not.toContain("node=");
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

it("hides the previous authority and download while Back resolves in the mounted drawer", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  let pending = false;
  let release!: (response: Response) => void;
  api((url) => pending && url === `/api/v1/versions/${oldID}`
    ? new Promise<Response>((resolve) => { release = resolve; }) : undefined);
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("1001 retained versions");
  const drawer = screen.getByRole("dialog");
  await fireEvent.click(screen.getByText("Revision 1001").closest("button")!);
  await screen.findByText(currentID);
  pending = true;
  history.back();
  await waitFor(() => expect(release).toBeTypeOf("function"));
  expect(location.hash).toBe(link());
  expect(screen.getByRole("dialog")).toBe(drawer);
  const previousAuthority = screen.queryByText(currentID) !== null;
  const download = screen.queryByRole("button", { name: "Download this version" }) !== null;
  await act(() => release(Response.json(oldVersion)));
  await screen.findByText(oldID);
  expect(previousAuthority).toBe(false);
  expect(download).toBe(false);
});

it("refreshes trash status on same-node browser navigation", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  let changed = false;
  api((url) => changed && url === "/api/v1/nodes/42"
    ? Response.json({ ...node, trashed_at: "2026-10-01T00:00:00Z" }) : undefined);
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("1001 retained versions");
  await fireEvent.click(screen.getByText("Revision 1001").closest("button")!);
  await screen.findByText(currentID);
  changed = true;
  history.back();
  await screen.findByText("Trash", { selector: "span" });
  expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
});

it("does not push history when clicking the displayed version", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("1001 retained versions");
  await fireEvent.click(screen.getByText("Revision 1001").closest("button")!);
  await screen.findByText(currentID);
  const push = vi.spyOn(history, "pushState");
  await fireEvent.click(within(screen.getByRole("region", { name: "Retained versions" })).getByText("Revision 1001").closest("button")!);
  expect(push).not.toHaveBeenCalled();
});

it("returns focus to the photo viewer after closing linked history", async () => {
  history.replaceState(null, "", `/photos#${credentials}photo=photo-1`);
  storage();
  api((url, init) => photoMetadataResponse(url, init) ?? (url.endsWith("/photos/hidden") ? Response.json({ configured: false }) : url.endsWith("/photos/assets/query") ? Response.json({ items: [photo(1)], total: 1 }) : undefined));
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

it("preserves server detail for unexpected bad requests during link resolution", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api((url) => url === "/api/v1/nodes/42"
    ? Response.json({ detail: "Request rejected by the server" }, { status: 400 }) : undefined);
  render(App);
  expect(await screen.findByText("Request rejected by the server")).toBeTruthy();
});

it("ignores an old drawer close after another panel takes ownership", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  render(App);
  await screen.findByText(oldID);
  const close = screen.getByRole("button", { name: "Close version history" });
  const storage = screen.getByRole("button", { name: "Storage status" });
  await act(() => {
    storage.click();
    close.click();
  });
  expect(await screen.findByRole("dialog", { name: "Physical storage status" })).toBeTruthy();
  expect(location.hash).toBe(link());
});
