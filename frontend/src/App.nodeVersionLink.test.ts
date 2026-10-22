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

it.each(["purged version", "server error message"])(
  "shows unavailable for %s without current substitution",
  async (kind) => {
    history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
    api((url) => {
      if (kind === "server error message" && url === "/api/v1/nodes/42")
        return Response.json({ detail: "Request rejected by the server" }, { status: 400 });
      if (url === `/api/v1/versions/${oldID}`)
        return kind === "purged version"
          ? Response.json({ detail: "missing" }, { status: 404 })
          : undefined;
      return undefined;
    });
    render(App);
    expect(
      await screen.findByText(
        kind === "server error message"
          ? "Request rejected by the server"
          : /node or version is missing or purged/,
      ),
    ).toBeTruthy();
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
        items: [{ ...node, id: 99, name: "other.txt" }],
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

it.each([
  { action: "Lock web session", linked: true },
  { action: "Lock web session", linked: false },
])("clears other fragment intent on $action with linked=$linked", async ({ action, linked }) => {
  const selection = linked ? link().slice(1) + "&" : "";
  history.replaceState(
    null,
    "",
    `/#${credentials}${selection}query=%7B%7D&photo=example&other=value`,
  );
  api();
  render(App);
  if (linked) await screen.findByText(oldID);
  await fireEvent.click(await screen.findByRole("button", { name: action }));
  await waitFor(() => expect(screen.queryByRole("button", { name: action })).toBeNull());
  expect(location.hash).toBe(linked ? link() : "");
});

it.each(["changed link key", "unchanged link key"])(
  "handles pending resolution after hash navigation with a %s",
  async (navigation) => {
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
    const changedKey = navigation === "changed link key";
    await act(() => {
      history.pushState(null, "", changedKey ? `/${link(currentID)}` : `/${link()}&query=%7B%7D`);
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    if (changedKey) {
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
    } else {
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
    }
  },
);

it("reopens exact versions on browser back and forward in the mounted drawer", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  const replacementID = "44444444-4444-4444-8444-444444444444";
  const replacement = version(replacementID, 1002, "content_replace", "c");
  let changed = false;
  let pending = false;
  let release!: (response: Response) => void;
  const fetchMock = api((url) => {
    if (changed && url === "/api/v1/nodes/42")
      return Response.json({ ...node, revision: 1002, current_version_id: replacementID });
    if (url === "/api/v1/nodes/42/versions?limit=1000&offset=0")
      return Response.json({
        items: changed ? [replacement, currentVersion, oldVersion] : [currentVersion, oldVersion],
        total: changed ? 3 : 2,
        limit: 1000,
        offset: 0,
      });
    if (pending && url === `/api/v1/versions/${oldID}`)
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    return undefined;
  });
  render(App);
  await screen.findByText(oldID);
  await screen.findByText("2 retained versions");
  const drawer = screen.getByRole("dialog");
  fetchMock.mockClear();
  await fireEvent.click(screen.getByText("Revision 1001").closest("button")!);
  await screen.findByText(currentID);
  expect(location.hash).toBe(link(currentID));
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).includes("/versions?")),
  ).toHaveLength(0);
  const push = vi.spyOn(history, "pushState");
  await fireEvent.click(
    within(screen.getByRole("region", { name: "Retained versions" }))
      .getByText("Revision 1001")
      .closest("button")!,
  );
  expect(push).not.toHaveBeenCalled();
  changed = true;
  pending = true;
  history.back();
  await waitFor(() => expect(release).toBeTypeOf("function"));
  expect(location.hash).toBe(link());
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(screen.queryByText(currentID)).toBeNull();
  expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
  await act(() => release(Response.json(oldVersion)));
  await screen.findByText(oldID);
  const timeline = within(screen.getByRole("region", { name: "Retained versions" }));
  await timeline.findByText("Revision 1002");
  expect(
    within(timeline.getByText("Current").closest("button")!).getByText("Revision 1002"),
  ).toBeTruthy();
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual(
    expect.arrayContaining(["/api/v1/nodes/42", `/api/v1/versions/${oldID}`]),
  );
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).includes("/versions?")),
  ).toHaveLength(1);
  pending = false;
  history.forward();
  await waitFor(() => expect(location.hash).toBe(link(currentID)));
  await screen.findByText(currentID);
  expect(screen.getByRole("dialog")).toBe(drawer);
  expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain(
    `/api/v1/versions/${currentID}`,
  );
  expect(location.hash).toBe(link(currentID));
  await fireEvent.click(timeline.getByText("Revision 1002").closest("button")!);
  await within(drawer).findByText(replacementID);
  expect(location.hash).toBe(link(replacementID));
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).includes("/versions?")),
  ).toHaveLength(1);
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

it("uses the newer linked node after Back when the selected directory row is stale and the file was renamed", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  let changed = false;
  const fetchMock = api((url) => {
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
        path: "/reports/renamed.txt",
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
  await fireEvent.click((await screen.findByText("Revision 1001")).closest("button")!);
  await within(screen.getByRole("dialog")).findByText(currentID);
  changed = true;
  history.back();
  await screen.findByText(oldID);
  const drawer = within(screen.getByRole("dialog"));
  expect(drawer.getByText("renamed.txt")).toBeTruthy();
  fetchMock.mockClear();
  await fireEvent.click(drawer.getByRole("button", { name: "Download this version" }));
  await drawer.findByText("Download unavailable in fixture");
  const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/daemon/web-download");
  expect(JSON.parse(String(call?.[1]?.body))).toMatchObject({
    revision: 1002,
    version_id: oldID,
  });
});

it("uses the tag removal receipt revision for a linked version download after Back removes the selected row", async () => {
  history.replaceState(null, "", `/#${credentials}`);
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const vaultID = "55555555-5555-4555-8555-555555555555";
  const tag = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 2,
  };
  const other = { ...node, id: 99, name: "other.txt", path: "/reports/other.txt" };
  const receipt = {
    node: { ...node, revision: 1002 },
    tag: { ...tag, revision: 2, assignment_count: 1 },
    changed: true,
  };
  localStorage.setItem(
    `docbank:tag-hotkeys:v1:${vaultID}`,
    JSON.stringify({ version: 1, bindings: { "1": tag.id } }),
  );
  let removed = false;
  let release!: (response: Response) => void;
  const fetchMock = api((url, init) => {
    if (url.startsWith("/api/v1/audit/status"))
      return Response.json({
        enabled: false,
        vault_id: vaultID,
        operation_sequence_high_water: 0,
        allocation_entry_count: 0,
        scopes: [],
      });
    if (url.includes("/children"))
      return Response.json({
        directory: root,
        items: [node, other],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return Response.json({
        items: [removed ? receipt.tag : tag],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === `/api/v1/tags/${tag.id}/nodes?limit=1000&offset=0&live_only=true`)
      return Response.json({
        items: (removed ? [other] : [node, other]).map((item) => ({ node: item, path: item.path })),
        total: removed ? 1 : 2,
        limit: 1000,
        offset: 0,
      });
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
    if (url === "/api/v1/nodes/99") return Response.json(other);
    if (url === "/api/v1/nodes/42/versions?limit=1000&offset=0")
      return Response.json({
        items: [currentVersion, oldVersion],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/daemon/web-download")
      return JSON.parse(String(init?.body)).revision === 1002
        ? Response.json({ detail: "Download unavailable in fixture" }, { status: 503 })
        : Response.json({ detail: "Node revision changed" }, { status: 409 });
    return undefined;
  });
  render(App);
  await screen.findByRole("cell", { name: "example.txt" });
  await fireEvent.click(
    await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (2)" }));
  await fireEvent.click(await screen.findByRole("cell", { name: "/reports/example.txt" }));
  await screen.findByText("1 assigned");
  await act(() => {
    location.hash = link();
  });
  await screen.findByText(oldID);
  await fireEvent.click((await screen.findByText("Revision 1001")).closest("button")!);
  await within(screen.getByRole("dialog")).findByText(currentID);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await fireEvent.keyDown(window, { key: "1" });
  await waitFor(() => expect(release).toBeTypeOf("function"));
  history.back();
  await screen.findByText(oldID);
  await act(() => {
    removed = true;
    release(Response.json(receipt));
  });
  await screen.findByText("Removed tax from example.txt.");
  await waitFor(() =>
    expect(screen.queryByRole("cell", { name: "/reports/example.txt" })).toBeNull(),
  );
  const drawer = within(screen.getByRole("dialog"));
  fetchMock.mockClear();
  await fireEvent.click(drawer.getByRole("button", { name: "Download this version" }));
  await waitFor(() =>
    expect(
      fetchMock.mock.calls.some(([input]) => String(input) === "/api/daemon/web-download"),
    ).toBe(true),
  );
  const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/daemon/web-download");
  expect(JSON.parse(String(call?.[1]?.body))).toMatchObject({
    revision: 1002,
    version_id: oldID,
  });
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
  const viewer = await screen.findByRole("dialog", { name: /Photo viewer/ });
  screen.getByRole("button", { name: "Close photo viewer" }).focus();
  expect(history.state?.photoReturn).toBeTruthy();
  await act(() => {
    history.replaceState(history.state, "", `/photos#photo=photo-1&${link().slice(1)}`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await screen.findByText(oldID);
  await fireEvent.click((await screen.findByText("Revision 1001")).closest("button")!);
  await screen.findByText(currentID);
  await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
  await waitFor(() => expect(document.activeElement).toBe(viewer));
  await fireEvent.click(screen.getByRole("button", { name: "Close photo viewer" }));
  await waitFor(() => expect(location.hash).toBe(""));
  expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Close version history" })).toBeNull();
  expect(screen.getByRole("button", { name: "Open Photo 1.jpg" })).toBeTruthy();
});
