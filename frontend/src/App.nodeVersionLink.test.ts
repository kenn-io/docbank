import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { storage, photo, photoMetadataResponse } from "./photo-test-fixtures.js";
import { node as fileNode, root, version } from "./version-test-fixtures.js";
import type { Node } from "./generated/docbank.js";

const oldID = "11111111-1111-4111-8111-111111111111";
const currentID = "22222222-2222-4222-8222-222222222222";
const node: Node = {
  ...fileNode,
  name: "example.txt",
  kind: "file",
  path: "/example.txt",
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
          ...Array.from({ length: 999 }, (_, index) => ({
            ...version(
              `${(index + 1).toString(16).padStart(8, "0")}-3333-4333-8333-333333333333`,
              1000 - index,
              "content_replace",
              "b",
            ),
            size: currentVersion.size,
          })),
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

it("opens a node absent from directory rows at its linked historical version", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  render(App);
  expect(await screen.findByText(oldID)).toBeTruthy();
  await screen.findByText("1001 retained versions");
  expect(location.hash).toBe(link());
});

it.each([
  {
    kind: "purged version",
    hash: link(),
    url: `/api/v1/versions/${oldID}`,
    response: () => Response.json({ detail: "missing" }, { status: 404 }),
    message: /node or version is missing or purged/,
  },
  {
    kind: "server error message",
    hash: link(),
    url: "/api/v1/nodes/42",
    response: () => Response.json({ detail: "Request rejected by the server" }, { status: 400 }),
    message: "Request rejected by the server",
  },
  {
    kind: "malformed link",
    hash: "#node=42",
    url: "",
    response: () => undefined,
    message: /Invalid node\/version link/,
  },
  {
    kind: "trashed linked node",
    hash: link(),
    url: "/api/v1/nodes/42",
    response: () => Response.json({ ...node, path: undefined, trashed_at: "2026-10-01T00:00:00Z" }),
    message: "Trash",
  },
])(
  "shows $kind without current substitution or download",
  async ({ hash, url, response, message }) => {
    history.replaceState(null, "", `/#${credentials}${hash.slice(1)}`);
    api((requestURL) => (requestURL === url ? response() : undefined));
    render(App);
    await waitFor(() => expect(within(screen.getByRole("dialog")).getByText(message)).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    expect(screen.queryByText(currentID)).toBeNull();
  },
);

it("keeps the resolved linked selection when root rows arrive after sign-in", async () => {
  history.replaceState(null, "", `/${link()}`);
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
  await screen.findByText(oldID);
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await act(() =>
    release(
      Response.json({
        directory: root,
        items: [{ ...fileNode, id: 99, name: "other.txt" }],
        total: 1,
        limit: 1000,
        offset: 0,
      }),
    ),
  );
  await screen.findByRole("cell", { name: "other.txt" });
  expect(screen.getByText(oldID)).toBeTruthy();
  expect(location.hash).toBe(link());
});

it.each([`/api/v1/versions/${oldID}`, "/api/v1/nodes/42/versions?limit=1000&offset=0"])(
  "preserves linked intent through sign-in when %s forwards a session rejection",
  async (failingRequest) => {
    history.replaceState(null, "", `/${link()}`);
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
    await signIn();
    expect(await screen.findByText(oldID)).toBeTruthy();
  },
);

it.each([true, false])(
  "clears other fragment intent on Lock web session with linked=%s",
  async (linked) => {
    const selection = linked ? link().slice(1) + "&" : "";
    history.replaceState(
      null,
      "",
      `/#${credentials}${selection}query=%7B%7D&photo=example&other=value`,
    );
    api();
    render(App);
    if (linked) await screen.findByText(oldID);
    await fireEvent.click(await screen.findByRole("button", { name: "Lock web session" }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Lock web session" })).toBeNull(),
    );
    expect(location.hash).toBe(linked ? link() : "");
  },
);

it("aborts a pending lookup on a changed link and closes the drawer on link removal", async () => {
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
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input) === `/api/v1/versions/${currentID}`),
  ).toHaveLength(1);
  await act(() => {
    history.replaceState(null, "", "/#photo=example");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("keeps a pending lookup and the mounted drawer when the link key is unchanged", async () => {
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
  const drawer = screen.getByRole("dialog");
  await act(() => {
    history.pushState(null, "", `/${link()}&query=%7B%7D`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(oldSignal?.aborted).toBe(false);
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

it("reopens exact versions on Back and Forward and uses the renamed node instead of the stale directory row", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  let changed = false;
  let failLookup = false;
  let release!: (response: Response) => void;
  const fetchMock = api((url) => {
    if (url === `/api/v1/versions/${oldID}` && failLookup) {
      failLookup = false;
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }
    if (url.includes("/children"))
      return Response.json({ directory: root, items: [node], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/42/versions?limit=1000&offset=0")
      return Response.json({
        items: [currentVersion, oldVersion],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    if (changed && url === "/api/v1/nodes/42")
      return Response.json({
        ...node,
        revision: 1002,
        name: "renamed.txt",
        path: "/renamed.txt",
      });
    if (url === "/api/daemon/web-download")
      return Response.json({ detail: "Download unavailable in fixture" }, { status: 503 });
    return undefined;
  });
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "example.txt" }));
  await act(() => {
    location.hash = link();
  });
  await screen.findByText(oldID);
  const mountedDrawer = screen.getByRole("dialog");
  expect(location.hash).toBe(link());
  const drawer = within(mountedDrawer);
  const timeline = within(drawer.getByRole("region", { name: "Retained versions" }));
  await fireEvent.click((await timeline.findByText("Revision 1001")).closest("button")!);
  await within(mountedDrawer).findByText(currentID);
  expect(location.hash).toBe(link(currentID));
  const push = vi.spyOn(history, "pushState");
  await fireEvent.click(timeline.getByText("Revision 1001").closest("button")!);
  expect(push).not.toHaveBeenCalled();
  failLookup = true;
  history.back();
  await waitFor(() => expect(release).toBeTypeOf("function"));
  const historyLength = history.length;
  expect(location.hash).toBe(link());
  await fireEvent.click(timeline.getByText("Revision 1").closest("button")!);
  expect(push).not.toHaveBeenCalled();
  expect(location.hash).toBe(link());
  expect(history.length).toBe(historyLength);
  await act(() =>
    release(Response.json({ detail: "Version lookup temporarily unavailable" }, { status: 503 })),
  );
  await drawer.findByText("Version lookup temporarily unavailable");
  expect(location.hash).toBe(link());
  await fireEvent.click(timeline.getByText("Revision 1001").closest("button")!);
  expect(drawer.queryByText("Version lookup temporarily unavailable")).toBeNull();
  expect(drawer.getByText(currentID)).toBeTruthy();
  expect(drawer.getByRole("button", { name: "Download this version" })).toBeTruthy();
  changed = true;
  history.back();
  await within(mountedDrawer).findByText(oldID);
  expect(location.hash).toBe(link());
  expect(drawer.getByText("renamed.txt")).toBeTruthy();
  fetchMock.mockClear();
  await fireEvent.click(drawer.getByRole("button", { name: "Download this version" }));
  await drawer.findByText("Download unavailable in fixture");
  const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/daemon/web-download");
  expect(JSON.parse(String(call?.[1]?.body))).toMatchObject({
    revision: 1002,
    version_id: oldID,
  });
  history.forward();
  await within(mountedDrawer).findByText(currentID);
  expect(location.hash).toBe(link(currentID));
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
    location.hash = `#photo=photo-1&${link().slice(1)}`;
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
