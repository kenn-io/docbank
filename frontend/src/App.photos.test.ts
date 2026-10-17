import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import {
  photoMetadataResponse,
  preparedPhoto,
  photo,
  mapPhoto,
  photoAlbum,
  albumResponse,
  savedSmartSet,
  storage,
} from "./photo-test-fixtures.js";
import * as api from "./generated/docbank.js";
import * as photoEvents from "./photoEvents.js";

const realWatchPhotoEvents = photoEvents.watchPhotoEvents;

beforeEach(() => vi.spyOn(photoEvents, "watchPhotoEvents").mockReturnValue(() => {}));
import * as photoWorkspace from "./PhotosWorkspace.svelte";
import App from "./App.svelte";
import { Photos } from "./photos.svelte.js";
import { VerifiedUploadChannel } from "./upload.js";
import { parseQuery, queryFingerprint, type Query } from "./query.js";
import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";

// Answers background filter-count requests so tests that predate counts see only their own reads.
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => unknown) =>
  vi.stubGlobal(
    "fetch",
    (url: string, init: RequestInit) =>
      photoMetadataResponse(url, init) ??
      (url.endsWith("/photos/assets/query") && JSON.parse(String(init?.body ?? "{}")).facets?.length
        ? Promise.resolve(Response.json({ items: [], total: 0, facets: [] }))
        : fetcher(url, init)),
  );

const map = vi.hoisted(() => ({
  props: undefined as
    | {
        items: ReturnType<typeof mapPhoto>[];
        onvisible: (rows: ReturnType<typeof mapPhoto>[]) => void;
      }
    | undefined,
}));
vi.mock("./PhotoMap.svelte", () => ({
  default: (_anchor: unknown, props: NonNullable<typeof map.props>) => {
    map.props = props;
    return {};
  },
}));

const originalCreateObjectURL = Object.getOwnPropertyDescriptor(URL, "createObjectURL");
const originalRevokeObjectURL = Object.getOwnPropertyDescriptor(URL, "revokeObjectURL");
afterEach(() => {
  cleanup();
  map.props = undefined;
  document.querySelectorAll('meta[name="docbank-map-tiles"]').forEach((meta) => meta.remove());
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  if (originalCreateObjectURL)
    Object.defineProperty(URL, "createObjectURL", originalCreateObjectURL);
  else Reflect.deleteProperty(URL, "createObjectURL");
  if (originalRevokeObjectURL)
    Object.defineProperty(URL, "revokeObjectURL", originalRevokeObjectURL);
  else Reflect.deleteProperty(URL, "revokeObjectURL");
  localStorage.clear();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  new Photos("scoped", vi.fn()).setDensity("comfortable");
});

it("removes a delayed direct-link photo fragment on tab switch without history.back", async () => {
  history.replaceState(
    null,
    "",
    "/photos/hidden#web_session=synthetic&web_upload_secret=proof&photo=photo-1",
  );
  storage();
  let finish!: (response: Response) => void;
  stubPhotoFetch(
    vi.fn(async (url: string) => {
      if (url.endsWith("/photos/hidden"))
        return new Promise<Response>((resolve) => (finish = resolve));
      return Response.json({ items: [], total: 0 });
    }),
  );
  const back = vi.spyOn(history, "back");
  render(App);
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(history.state?.photoReturn).toBeUndefined();
  expect(location.hash).toBe("#photo=photo-1");
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  expect(back).not.toHaveBeenCalled();
  expect(location.hash).toBe("");
  finish(Response.json({ configured: true }));
});

it("locks once when an authenticated photo direct link starts in a hidden tab", async () => {
  history.replaceState(
    null,
    "",
    "/photos/hidden#web_session=synthetic&web_upload_secret=proof&photo=photo-1",
  );
  storage();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  const fetcher = vi.fn(async (_url: string) =>
    Response.json({ configured: true, items: [], total: 0 }),
  );
  stubPhotoFetch(fetcher);
  const locks = () => fetcher.mock.calls.filter(([url]) => url.endsWith("/photos/hidden/lock"));
  render(App);
  await waitFor(() => expect(locks().length).toBeGreaterThan(0));
  await act(async () => {});
  expect(location.hash).toBe("");
  expect(locks()).toHaveLength(1);
  await fireEvent(window, new Event("pagehide"));
  expect(locks()).toHaveLength(1);
});

it("locks once when sign-in completes after the Photos tab was hidden", async () => {
  history.replaceState(null, "", "/photos/hidden");
  storage();
  let finish!: (response: Response) => void;
  let locks = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url.endsWith("/web-auth")) return Response.json({ enabled: true });
      if (url.endsWith("/login")) return new Promise<Response>((resolve) => (finish = resolve));
      if (url.endsWith("/photos/hidden/lock")) locks++;
      return Response.json({ configured: true, items: [], total: 0 });
    }),
  );
  render(App);
  await fireEvent.input(await screen.findByLabelText("API key"), {
    target: { value: "synthetic-key" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  finish(Response.json({ enabled: true, session: { token: "synthetic", upload_secret: "proof" } }));
  await screen.findByRole("button", { name: "Sign out" });
  await waitFor(() => expect(locks).toBe(1));
  await fireEvent(window, new Event("pagehide"));
  await waitFor(() => expect(locks).toBe(1));
});

it.each(["setup", "unlock", "change", "disable"])(
  "preserves submitted %s passcodes when navigating to Library behind a held leave lock",
  async (kind) => {
    history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
    storage();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    let finish!: () => void;
    const requests: { kind: string; body: unknown }[] = [];
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/hidden/lock")) {
          await new Promise<void>((resolve) => (finish = resolve));
          return Response.json({});
        }
        if (init?.method === "POST" && /\/(setup|unlock|change|disable)$/.test(url))
          requests.push({ kind: url.split("/").at(-1)!, body: JSON.parse(String(init.body)) });
        return Response.json({ configured: kind !== "setup", items: [], total: 0 });
      }),
    );
    render(App);
    await screen.findByRole("button", { name: kind === "setup" ? "Set passcode" : "Unlock" });
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    if (kind === "change" || kind === "disable") {
      await fireEvent.click(screen.getByText("Manage passcode"));
      await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), {
        target: { value: "submitted-current" },
      });
      if (kind === "change")
        await fireEvent.input(screen.getByLabelText("New passcode", { exact: true }), {
          target: { value: "submitted-new" },
        });
    } else
      await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), {
        target: { value: "submitted-passcode" },
      });
    await fireEvent.click(
      screen.getByRole("button", {
        name:
          kind === "setup"
            ? "Set passcode"
            : kind === "unlock"
              ? "Unlock"
              : kind === "change"
                ? "Change passcode"
                : "Disable Hidden",
      }),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Library" }));
    expect(location.pathname).toBe("/photos");
    expect(requests).toEqual([]);
    finish();
    await waitFor(() => expect(requests).toHaveLength(kind === "setup" ? 2 : 1));
    expect(requests).toEqual(
      kind === "setup"
        ? [
            { kind: "setup", body: { passcode: "submitted-passcode" } },
            { kind: "unlock", body: { passcode: "submitted-passcode" } },
          ]
        : [
            {
              kind,
              body:
                kind === "change"
                  ? { passcode: "submitted-current", new_passcode: "submitted-new" }
                  : { passcode: kind === "disable" ? "submitted-current" : "submitted-passcode" },
            },
          ],
    );
  },
);

it.each(["unhide", "trash", "rejects"])(
  "finishes every submitted %s before locking when the tab is hidden",
  async (action) => {
    history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
    storage();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    let finish!: () => void;
    let locked = false;
    const completed: string[] = [];
    stubPhotoFetch(
      vi.fn(async (url: string) => {
        if (url.endsWith("/photos/hidden/lock")) {
          locked = true;
          completed.push("lock");
          return Response.json({});
        }
        if (url.endsWith("/photos/hidden"))
          return Response.json({
            configured: true,
            expires_at: "2099-01-01T00:00:00Z",
            expires_in: 300,
          });
        if (url.endsWith("/photos/rejects/preflight"))
          return Response.json({
            targets: [1, 2, 3].map((i) => ({
              asset_id: `photo-${i}`,
              revision: 1,
              member_revision: 1,
            })),
            photos: 3,
            files: 3,
            unchanged: 0,
            mixed: [],
            mixed_count: 0,
            selection_limit: 64,
          });
        if (url.endsWith("/photos/rejects/trash")) {
          await new Promise<void>((resolve) => (finish = resolve));
          if (locked)
            return Response.json(
              { detail: "Hidden locked", code: "hidden_locked" },
              { status: 403 },
            );
          completed.push("photo-1", "photo-2", "photo-3");
          return Response.json({ moved: [...completed] });
        }
        const target = url.match(new RegExp(`/assets/(photo-\\d+)/${action}$`))?.[1];
        if (target) {
          if (target === "photo-1") await new Promise<void>((resolve) => (finish = resolve));
          if (locked)
            return Response.json(
              { detail: "Hidden locked", code: "hidden_locked" },
              { status: 403 },
            );
          completed.push(target);
          return Response.json({ id: target, revision: 2 });
        }
        const items = [photo(1), photo(2), photo(3)].filter(
          (item) => !completed.includes(item.asset_id),
        );
        return Response.json({ items, total: items.length });
      }),
    );
    render(App);
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 3.jpg" }));
    if (action === "unhide") {
      await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
      await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
    } else if (action === "rejects") {
      await fireEvent.click(screen.getByRole("button", { name: "Move rejects" }));
      await fireEvent.click(await screen.findByRole("button", { name: "Move 3 to trash" }));
    } else {
      await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
      await fireEvent.click(
        within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole(
          "button",
          { name: "Move to trash" },
        ),
      );
    }
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    expect(screen.queryByTestId("photo-scroll")).toBeNull();
    expect(completed).toEqual([]);
    finish();
    await waitFor(() => expect(completed).toEqual(["photo-1", "photo-2", "photo-3", "lock"]));
  },
);

it("renders no hidden photo buttons after a failed leave lock and return", async () => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let locks = 0;
  let unlocked = true;
  let finish!: () => void;
  stubPhotoFetch(
    vi.fn(async (url: string) => {
      if (url.endsWith("/photos/hidden/lock")) {
        if (++locks === 1) return Response.json({ detail: "Lock unavailable" }, { status: 503 });
        await new Promise<void>((resolve) => (finish = resolve));
        unlocked = false;
      }
      if (url.endsWith("/photos/hidden"))
        return Response.json({
          configured: true,
          ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}),
        });
      return Response.json({ items: [photo(1)], total: 1 });
    }),
  );
  render(App);
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Hidden did not lock. Lock unavailable");
  visibility.mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull();
  expect(screen.queryByLabelText("Passcode", { exact: true })).toBeNull();
  expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Retry lock" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  finish();
  await waitFor(() =>
    expect(screen.queryByText("Hidden did not lock. Lock unavailable")).toBeNull(),
  );
  await screen.findByRole("button", { name: "Unlock" });
});

it.each(["hide", "close", "Documents after Photos", "Documents only", "failed lock"])(
  "locks Hidden when leaving %s",
  async (mode) => {
    history.replaceState(
      null,
      "",
      `${mode === "Documents only" ? "/" : "/photos"}#web_session=synthetic&web_upload_secret=proof`,
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
    let locks = 0;
    const fetcher = vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/hidden/lock")) {
        locks++;
        if (mode === "failed lock" && locks === 1)
          return Response.json({ detail: "Lock unavailable" }, { status: 503 });
        return Response.json({ configured: true });
      }
      if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
      if (url.endsWith("/photos/assets/query"))
        return Response.json({ items: [photo(1)], total: 1 });
      const root = { id: 1, kind: "dir", name: "", revision: 1, path: "/" };
      if (url.includes("/children")) return Response.json({ directory: root, items: [], total: 0 });
      if (url.includes("/nodes/1") || url.startsWith("/api/v1/path")) return Response.json(root);
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    });
    stubPhotoFetch(fetcher);
    render(App);
    if (mode === "Documents only") await screen.findByRole("button", { name: "Photos" });
    else await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    if (mode === "Documents after Photos" || mode === "failed lock")
      await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    const visibility = vi.spyOn(document, "visibilityState", "get");
    if (mode !== "close") {
      visibility.mockReturnValue("hidden");
      await fireEvent(document, new Event("visibilitychange"));
      await fireEvent(document, new Event("visibilitychange"));
    }
    await fireEvent(window, new Event("pagehide"));
    if (mode === "Documents only") {
      expect(locks).toBe(0);
      return;
    }
    await waitFor(() => expect(locks).toBe(1));
    expect(
      fetcher.mock.calls.find(([url]) => url.endsWith("/photos/hidden/lock"))?.[1],
    ).toMatchObject({ keepalive: true });
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    await fireEvent(window, new Event("pageshow"));
    if (mode === "failed lock") {
      expect(location.pathname).toBe("/");
      expect(
        (await screen.findByText("Hidden did not lock. Lock unavailable")).getAttribute("role"),
      ).toBe("alert");
      await fireEvent.click(screen.getByRole("button", { name: "Retry lock" }));
      await waitFor(() =>
        expect(screen.queryByText("Hidden did not lock. Lock unavailable")).toBeNull(),
      );
      expect(locks).toBe(2);
    } else {
      await fireEvent(window, new Event("pagehide"));
      await waitFor(() => expect(locks).toBe(2));
    }
  },
);

it.each(["server", "stale first page"])(
  "refreshes an album opened again during a delayed membership write with a %s response",
  async (outcome) => {
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
    Element.prototype.scrollIntoView = vi.fn();
    const album = photoAlbum();
    let included = false;
    let finish!: () => void;
    let releaseStale: (() => void) | undefined;
    let scopedReads = 0;
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/albums"))
          return albumResponse([{ ...album, included_count: included ? 1 : 0 }]);
        if (url.endsWith("/photos/assets/query")) {
          const scoped = JSON.parse(init.body as string).query.filters?.set_ids?.includes(album.id);
          if (outcome === "stale first page" && scoped && ++scopedReads === 1) {
            await new Promise<void>((resolve) => (releaseStale = resolve));
            return albumResponse({ items: [], total: 0 });
          }
          return albumResponse({
            items: scoped && !included ? [] : [photo(1)],
            total: scoped && !included ? 0 : 1,
          });
        }
        if (url.endsWith(`/photos/albums/${album.id}/members/add`)) {
          await new Promise<void>((resolve) => (finish = resolve));
          included = true;
          return outcome === "server"
            ? albumResponse({ detail: "Add unavailable" }, 503)
            : albumResponse({ ...album, revision: 2 });
        }
        if (url.includes("/nodes/1"))
          return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
        return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    await fireEvent.mouseDown(await screen.findByRole("option", { name: "Trip" }));
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
    if (outcome === "stale first page") {
      await waitFor(() => expect(releaseStale).toBeTypeOf("function"));
    } else {
      await screen.findByText("This album is empty");
      await fireEvent.click(screen.getByRole("button", { name: "Library" }));
      await screen.findByRole("button", { name: "Select Photo 1.jpg" });
      await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
      await screen.findByText("This album is empty");
    }
    finish();
    if (outcome === "stale first page") {
      await waitFor(() => expect(scopedReads).toBe(2));
      releaseStale!();
    }
    if (outcome !== "stale first page")
      await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    await screen.findByText("1 photos · 1 loaded", undefined, { timeout: 5000 });
    expect(screen.queryByText("This album is empty")).toBeNull();
  },
);

it("reloads Library album counts after adding photos to an album", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum();
  let included = false;
  let countRequests = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/albums"))
        return albumResponse([{ ...album, included_count: included ? 1 : 0 }]);
      if (url.endsWith("/photos/assets/query")) {
        if (!JSON.parse(init.body as string).facets?.length)
          return albumResponse({ items: [photo(1)], total: 1 });
        countRequests++;
        const values = included
          ? [{ key: album.id, label: album.name, count: 1, selected: false }]
          : [];
        return albumResponse({
          items: [],
          total: 1,
          facets: [
            {
              dimension: "set",
              available: true,
              reason: "",
              total: 1,
              missing: 0,
              other: 0,
              values,
            },
          ],
        });
      }
      if (url.endsWith(`/photos/albums/${album.id}/members/add`)) {
        included = true;
        return albumResponse({ ...album, revision: 2 });
      }
      if (url.includes("/nodes/1"))
        return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
      return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
    }),
  );
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await waitFor(() => expect(countRequests).toBe(1));
  expect(screen.queryByRole("button", { name: `${album.name}, 1 photos` })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: album.name }));
  await within(screen.getByRole("group", { name: "Albums facet" })).findByRole(
    "button",
    { name: `${album.name}, 1 photos` },
    { timeout: 5000 },
  );
  expect(countRequests).toBe(2);
});

it("opens an album from the index, preserves its sort scope, and unstars without refetching", async () => {
  history.replaceState(null, "", "/photos/albums#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ starred: true, member_count: 1, included_count: 1 });
  const queries: unknown[] = [];
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
    if (url.endsWith(`/photos/albums/${album.id}`) && init.method === "PUT")
      return albumResponse({ ...album, revision: 2, starred: false });
    if (url.endsWith("/photos/assets/query")) {
      queries.push(JSON.parse(init.body as string).query);
      return new Response(JSON.stringify({ items: [photo(1)], total: 1 }));
    }
    if (url.includes("/nodes/1"))
      return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  stubPhotoFetch(fetcher);
  render(App);
  await fireEvent.click(await screen.findByRole("link", { name: /Trip.*1 photo/ }));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect(queries[0]).toMatchObject({
    filters: { set_ids: [album.id] },
    sort: { field: "added_time", direction: "desc" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: /Sort photos/ }));
  await fireEvent.click(await screen.findByRole("option", { name: "Imported" }));
  await waitFor(() =>
    expect(queries.at(-1)).toMatchObject({
      filters: { set_ids: [album.id] },
      sort: { field: "import_time", direction: "desc" },
    }),
  );
  expect(screen.getByRole("combobox", { name: "Sort photos: Imported" })).toBeTruthy();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  album.included_count = 7;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  expect(await screen.findByRole("combobox", { name: "Sort photos: Imported" })).toBeTruthy();
  await screen.findByText("1 selected photo");
  await waitFor(() =>
    expect(screen.getByRole("button", { name: /Trip/ }).textContent).toContain("7"),
  );
  const browseCount = queries.length;
  await fireEvent.click(screen.getByRole("button", { name: "Unstar album" }));
  await waitFor(() =>
    expect(
      fetcher.mock.calls.some(
        ([url, init]) => url.endsWith(`/photos/albums/${album.id}`) && init.method === "PUT",
      ),
    ).toBe(true),
  );
  await waitFor(() =>
    expect(
      (screen.getByRole("button", { name: "Unstar album" }) as HTMLButtonElement).disabled,
    ).toBe(false),
  );
  expect(queries).toHaveLength(browseCount);
});

it.each(["document trash", "delayed photo trash", "restore"])(
  "refreshes albums and both started photo views after %s",
  async (operation) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
    Element.prototype.scrollIntoView = vi.fn();
    const album = photoAlbum({ member_count: 1, included_count: operation === "restore" ? 0 : 1 });
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    const node = {
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
      revision: 1,
      size: 0,
    };
    const root = { ...node, id: 1, name: "", kind: "dir", path: "/" };
    const reports = {
      ...node,
      id: 2,
      parent_id: 1,
      name: "Reports",
      kind: "dir",
      path: "/Reports",
    };
    const file = {
      ...node,
      id: 3,
      parent_id: 2,
      name: "photo-1.jpg",
      kind: "file",
      current_version_id: "11111111-1111-4111-8111-111111111111",
      blob_hash: "a".repeat(64),
      size: 74,
      mime_type: "image/jpeg",
      path: "/Reports/photo-1.jpg",
    };
    const otherAlbum = photoAlbum({ id: "22222222-2222-4222-8222-000000000068", name: "Other" });
    let finishTrash!: () => void;
    let trashed = operation === "restore";
    const reads = { albums: 0, album: 0, library: 0 };
    stubPhotoFetch(
      vi.fn(async (url: string, init?: RequestInit) => {
        const json = (value: unknown) =>
          new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
        if (url.endsWith("/photos/albums")) {
          reads.albums++;
          return json([{ ...album, included_count: trashed ? 0 : 1 }, otherAlbum]);
        }
        if (url.endsWith("/photos/assets/query")) {
          reads[JSON.parse(init!.body as string).query.filters?.set_ids ? "album" : "library"]++;
          return json({ items: trashed ? [] : [photo(1)], total: trashed ? 0 : 1 });
        }
        if (url === "/api/v1/path?path=%2F") return json(root);
        if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
          return json({ directory: root, items: [reports], total: 1, limit: 1000, offset: 0 });
        if (url === "/api/v1/nodes/2/children?limit=1000&offset=0")
          return json({
            directory: reports,
            items: trashed ? [] : [file],
            total: trashed ? 0 : 1,
            limit: 1000,
            offset: 0,
          });
        if (url.endsWith("/trash") && init?.method === "POST") {
          if (operation === "delayed photo trash")
            await new Promise<void>((resolve) => (finishTrash = resolve));
          trashed = true;
          return json(
            url.includes("/photos/assets/")
              ? { id: "photo-1", revision: 2 }
              : { ...file, revision: 2, trashed_at: "2026-07-28T12:01:00Z" },
          );
        }
        if (url.startsWith("/api/v1/trash?"))
          return json({
            items: trashed ? [{ ...file, trashed_at: "2026-07-28T12:01:00Z" }] : [],
            total: trashed ? 1 : 0,
            limit: 1000,
            offset: 0,
          });
        if (url.endsWith("/nodes/3/restore")) {
          trashed = false;
          return json({ ...file, revision: 2 });
        }
        if (url === "/api/v1/nodes/3") return json(file);
        if (url.startsWith("/api/v1/audit/status")) return json({ enabled: false, scopes: [] });
        return json({ items: [], total: 0, limit: 1000, offset: 0 });
      }),
    );
    render(App);
    if (operation === "restore") await screen.findByText("Your photo library is empty");
    else await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
    await screen.findByText(
      operation === "restore" ? "This album is empty" : "1 photos · 1 loaded",
    );
    const before = { ...reads };
    if (operation === "document trash") {
      await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
      await fireEvent.dblClick(await screen.findByRole("cell", { name: "Reports" }));
      await screen.findByRole("cell", { name: "photo-1.jpg" });
      await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
      await fireEvent.click(
        within(screen.getByRole("dialog", { name: "Move photo-1.jpg to trash" })).getByRole(
          "button",
          { name: "Move to trash" },
        ),
      );
    } else if (operation === "restore") {
      await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
      await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
      await fireEvent.click(
        within(screen.getByRole("dialog", { name: "Restore photo-1.jpg from trash" })).getByRole(
          "button",
          { name: "Restore" },
        ),
      );
    } else {
      await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
      await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
      await fireEvent.click(
        within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole(
          "button",
          { name: "Move to trash" },
        ),
      );
    }
    if (operation === "delayed photo trash") {
      await waitFor(() => expect(finishTrash).toBeTypeOf("function"));
      await fireEvent.click(screen.getByRole("button", { name: /^Other/ }));
      await screen.findByRole("heading", { name: "Other" });
      await screen.findByText("1 photos · 1 loaded");
      Object.assign(before, reads);
      finishTrash();
    }
    await waitFor(() => {
      for (const key of ["albums", "album", "library"] as (keyof typeof reads)[])
        expect(reads[key]).toBeGreaterThan(before[key]);
    });
    if (operation === "document trash")
      await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    if (operation === "restore")
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    else await screen.findByText("This album is empty");
  },
);

it.each([false, true])(
  "retains failed selection and browsing position through partial visibility changes, hidden=%s",
  async (hidden) => {
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    const revoke = vi.fn();
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL() {
          return "blob:synthetic";
        }
        static revokeObjectURL = revoke;
      },
    );
    const stored = storage();
    const writes = vi.spyOn(Photos.prototype, "setHidden");
    const polls = new Map<number, () => void>();
    let nextPoll = 0;
    const interval = globalThis.setInterval,
      clear = globalThis.clearInterval;
    vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
      if (delay !== 2000) return interval(callback, delay, ...args);
      const id = --nextPoll;
      polls.set(id, callback as () => void);
      return id as unknown as ReturnType<typeof setInterval>;
    });
    vi.spyOn(globalThis, "clearInterval").mockImplementation((id) => {
      if (!polls.delete(Number(id))) clear(id);
    });
    const poll = async () => {
      for (const callback of polls.values()) callback();
      await new Promise((resolve) => setTimeout(resolve, 0));
    };
    let changed = false;
    const reads = [0, 0];
    let finish!: (response: Response) => void;
    const items = Array.from({ length: 60 }, (_, index) => ({
      ...photo(index + 1, new Date(Date.UTC(2025, 5, 1, 12, 60 - index)).toISOString()),
      previews: { grid: { state: "ready", generation_id: "synthetic" } },
    }));
    stubPhotoFetch(
      vi.fn(async (url: string, options?: RequestInit) => {
        if (url.endsWith("/photos/hidden"))
          return Response.json({
            configured: true,
            expires_at: "2099-01-01T00:00:00Z",
            expires_in: 300,
            change_id: changed ? "second" : "first",
          });
        if (url.endsWith("/assets/query")) {
          const scope = !!JSON.parse(String(options?.body)).hidden;
          reads[Number(scope)]++;
          const rows =
            hidden && !scope ? (changed ? [items[0]] : []) : changed ? items.slice(1) : items;
          return Response.json({ items: rows, total: rows.length });
        }
        if (url.includes("/previews/")) return new Response("synthetic-jpeg");
        if (url.endsWith(`/assets/photo-1/${hidden ? "unhide" : "hide"}`)) {
          changed = true;
          return Response.json({ id: "photo-1", revision: 2 });
        }
        if (url.endsWith(`/assets/photo-2/${hidden ? "unhide" : "hide"}`))
          return new Promise<Response>((resolve) => (finish = resolve));
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    if (hidden) {
      await screen.findByText("Your photo library is empty");
      await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
    }
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
    await screen.findByRole("img", { name: "Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("combobox", { name: "Group photos: Months" }));
    await fireEvent.click(screen.getByRole("option", { name: "Capture sessions" }));
    await fireEvent.click(screen.getByRole("combobox", { name: "Grid density: Comfortable" }));
    await fireEvent.click(screen.getByRole("option", { name: "Compact" }));
    const scroll = screen.getByTestId("photo-scroll");
    scroll.scrollTop = 100;
    await fireEvent.scroll(scroll);
    const cacheName = stored.open.mock.calls[0][0];
    await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
    await fireEvent.click(
      await screen.findByRole("menuitem", { name: hidden ? "Unhide" : "Hide" }),
    );
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.queryByTestId("photo-scroll")).toBeNull();
    await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
    expect(revoke).toHaveBeenCalled();
    await poll();
    expect(reads[Number(hidden)]).toBe(1);
    expect(reads[Number(!hidden)]).toBe(hidden ? 1 : 0);
    expect(screen.queryByTestId("photo-scroll")).toBeNull();
    finish(Response.json({ detail: "Stale revision" }, { status: 412 }));
    await writes.mock.results[0].value;
    await poll();
    await screen.findByText("1 photo failed: Stale revision", {}, { timeout: 5000 });
    await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(100), {
      timeout: 5000,
    });
    expect(screen.getByRole("combobox", { name: "Group photos: Capture sessions" })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Grid density: Compact" })).toBeTruthy();
    expect(
      (
        (await screen.findByRole("checkbox", {
          name: "Select photo Photo 2.jpg",
        })) as HTMLInputElement
      ).checked,
    ).toBe(true);
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
    expect(screen.getByText("1 selected photo")).toBeTruthy();
    if (hidden) {
      expect(reads[0]).toBe(1);
      await fireEvent.click(screen.getByRole("button", { name: "Library" }));
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
      expect(screen.queryByRole("checkbox", { name: "Select photo Photo 2.jpg" })).toBeNull();
      expect(reads[0]).toBe(2);
    }
  },
);

it.each([false, true])(
  "keeps concealed rows hidden when Trash restore interrupts revalidation, failed=%s",
  async (failed) => {
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    let poll!: () => void;
    const interval = globalThis.setInterval;
    vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
      if (delay === 2000) {
        poll = callback as () => void;
        return 0 as unknown as ReturnType<typeof setInterval>;
      }
      return interval(callback, delay, ...args);
    });
    const refresh = vi.spyOn(Photos.prototype, "refresh");
    let changed = false;
    let reads = 0;
    let finishConceal!: (response: Response) => void;
    let finishRestore!: (response: Response) => void;
    let concealSignal!: AbortSignal;
    const node = {
      id: 2,
      parent_id: 1,
      name: "Restored.jpg",
      kind: "file",
      revision: 1,
      size: 10,
      path: "/Restored.jpg",
      trashed_at: "2026-07-28T12:01:00Z",
    };
    stubPhotoFetch(
      vi.fn(async (url: string, options?: RequestInit) => {
        if (url.endsWith("/photos/hidden"))
          return Response.json({ configured: true, change_id: changed ? "second" : "first" });
        if (url.endsWith("/assets/query")) {
          reads++;
          if (reads === 2) {
            concealSignal = options!.signal!;
            return new Promise<Response>((resolve) => (finishConceal = resolve));
          }
          if (reads === 3) return new Promise<Response>((resolve) => (finishRestore = resolve));
          return Response.json({ items: changed ? [] : [photo(1)], total: changed ? 0 : 1 });
        }
        if (url.startsWith("/api/v1/trash?"))
          return Response.json({ items: [node], total: 1, limit: 1000, offset: 0 });
        if (url.endsWith("/nodes/2/restore"))
          return Response.json({ ...node, revision: 2, trashed_at: null });
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    changed = true;
    poll();
    await waitFor(() => expect(finishConceal).toBeDefined());
    await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Restore Restored.jpg from trash" })).getByRole(
        "button",
        { name: "Restore" },
      ),
    );
    await waitFor(() => expect(finishRestore).toBeDefined());
    expect(concealSignal.aborted).toBe(true);
    finishConceal(Response.json({ items: [], total: 0 }));
    await refresh.mock.results[0].value;
    expect(screen.queryByTestId("photo-scroll")).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
    finishRestore(
      failed
        ? Response.json({ detail: "Replacement failed" }, { status: 503 })
        : Response.json({ items: [], total: 0 }),
    );
    await refresh.mock.results[1].value;
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
    if (failed) expect(screen.queryByTestId("photo-scroll")).toBeNull();
    poll();
    await screen.findByText("Your photo library is empty");
  },
);

async function failStatusReads(reads: () => number, count: number) {
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  for (const target = reads() + count; reads() < target; ) {
    const before = reads();
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(reads()).toBeGreaterThan(before), { timeout: 5000 });
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

it.each(["change", "failure", "background", "delayed page", "album route", "unrelated mutation"])(
  "validates retained photo state after %s",
  async (kind) => {
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    const album = photoAlbum({
      id: "22222222-2222-4222-8222-0000000000ac",
      included_count: 1,
      member_count: 1,
    });
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const revoke = vi.fn();
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL() {
          return "blob:synthetic";
        }
        static revokeObjectURL = revoke;
      },
    );
    const stored = storage();
    let changed = false;
    let statusReads = 0;
    let delay = false;
    let accessFailed = true;
    let finish!: (response: Response) => void;
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/hidden")) {
          statusReads++;
          if (changed && kind === "failure" && accessFailed) throw new Error("Status unavailable");
          return Response.json({
            configured: false,
            change_id: changed && kind !== "unrelated mutation" ? "second" : "first",
          });
        }
        if (url.endsWith("/photos/albums"))
          return Response.json([{ ...album, included_count: changed ? 0 : 1 }]);
        if (url.endsWith("/assets/query")) {
          const query = JSON.parse(String(init.body ?? "{}")).query;
          if (query.filters?.set_ids)
            return Response.json({ items: changed ? [] : [photo(1)], total: changed ? 0 : 1 });
          if (delay) {
            delay = false;
            return new Promise<Response>((resolve) => (finish = resolve));
          }
          return Response.json({
            items:
              changed && kind !== "failure"
                ? []
                : [
                    {
                      ...photo(1),
                      previews: { grid: { state: "ready", generation_id: "synthetic" } },
                    },
                  ],
            total: changed ? 0 : 1,
          });
        }
        if (url.includes("/previews/")) return new Response("synthetic-jpeg");
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    await screen.findByRole("img", { name: "Photo 1.jpg" });
    const cacheName = stored.open.mock.calls[0][0];
    await waitFor(() => expect(stored.data.get(cacheName)?.size).toBe(1));
    if (kind === "album route") {
      await fireEvent.click(await screen.findByRole("button", { name: /^Trip/ }));
      await screen.findByRole("main", { name: "Photo album" });
      await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    }
    if (kind === "delayed page") {
      delay = true;
      await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
      await waitFor(() => expect(finish).toBeDefined());
    }
    changed = true;
    if (kind === "album route") {
      await screen.findByText("This album is empty", undefined, { timeout: 5000 });
      delay = true;
      await fireEvent.click(screen.getByRole("button", { name: "Library" }));
      expect(location.pathname).toBe("/photos");
      await waitFor(() => expect(finish).toBeDefined());
    }
    if (kind === "unrelated mutation") {
      const scroll = screen.getByTestId("photo-scroll");
      scroll.scrollTop = 100;
      await fireEvent.scroll(scroll);
      await waitFor(() => expect(statusReads).toBeGreaterThan(1), { timeout: 5000 });
      expect(screen.getByTestId("photo-scroll")).toBe(scroll);
      expect(scroll.scrollTop).toBe(100);
      expect(screen.getByText("1 selected photo")).toBeTruthy();
      expect(stored.data.has(cacheName)).toBe(true);
      expect(revoke).not.toHaveBeenCalled();
      return;
    }
    if (kind === "background") {
      vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
      await fireEvent(document, new Event("visibilitychange"));
    }
    if (kind === "failure") {
      const reads = statusReads;
      await waitFor(() => expect(statusReads).toBeGreaterThan(reads), { timeout: 5000 });
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeTruthy();
      expect(screen.queryByText("Status unavailable")).toBeNull();
      await failStatusReads(() => statusReads, 2);
    }
    await waitFor(
      () => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull(),
      { timeout: 5000 },
    );
    expect(screen.queryByText("1 selected photo")).toBeNull();
    await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
    expect(revoke).toHaveBeenCalledWith("blob:synthetic");
    if (kind === "failure") {
      await screen.findByText("Status unavailable");
      accessFailed = false;
      await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
      expect(screen.getByText("1 selected photo")).toBeTruthy();
    }
    if (kind === "delayed page" || kind === "album route") {
      finish(
        Response.json(
          kind === "album route" ? { items: [], total: 0 } : { items: [photo(1)], total: 1 },
        ),
      );
      await screen.findByText("Your photo library is empty");
      expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
    }
  },
);

it("refreshes a remounted Hidden grid after a delayed unhide", async () => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  let changed = false;
  let finish!: (response: Response) => void;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, options?: RequestInit) => {
      const metadata = photoMetadataResponse(url, options);
      if (metadata) return metadata;
      if (url.endsWith("/photos/hidden"))
        return Response.json({
          configured: true,
          expires_at: "2099-01-01T00:00:00Z",
          expires_in: 300,
        });
      if (url.includes("/assets/query")) {
        const request = JSON.parse(String(options?.body));
        if (request.facets?.length) return Response.json({ items: [], total: 0, facets: [] });
        const hidden = request.hidden;
        const items = hidden && !changed ? [photo(1)] : [];
        return Response.json({ items, total: items.length });
      }
      if (url.endsWith("/assets/photo-1/unhide"))
        return new Promise<Response>((resolve) => (finish = resolve));
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    }),
  );
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  await waitFor(() => expect(finish).toBeDefined());
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await waitFor(() =>
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull(),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  changed = true;
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await screen.findByText("No hidden photos");
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
});

it("clears previews on sidebar and history departure while retaining eligible selection", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal(
    "URL",
    class extends URL {
      static createObjectURL() {
        return "blob:synthetic";
      }
      static revokeObjectURL() {}
    },
  );
  const stored = storage();
  let changed = false;
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response("[]");
    if (url.endsWith("/photos/hidden"))
      return Response.json({ configured: true, change_id: changed ? "second" : "first" });
    if (url.includes("/photos/assets/query")) {
      if (JSON.parse(init!.body as string).facets?.length)
        return new Response(JSON.stringify({ items: [], total: 1, facets: [] }));
      return new Response(
        JSON.stringify({
          items: (changed ? [] : [photo(1)]).map((item) => ({
            ...item,
            previews: { ...item.previews, grid: { state: "ready", generation_id: "synthetic" } },
          })),
          total: changed ? 0 : 1,
        }),
      );
    }
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    if (url.includes("/nodes/1"))
      return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await screen.findByRole("main", { name: "Photo library" });
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  const cacheName = stored.open.mock.calls[0][0];
  await waitFor(() => expect(stored.data.get(cacheName)?.size).toBe(1));
  const listings = () =>
    fetcher.mock.calls.filter(
      ([url, init]) =>
        url.includes("/photos/assets/query") && !JSON.parse(init!.body as string).facets?.length,
    ).length;
  const counts = () =>
    fetcher.mock.calls.filter(
      ([url, init]) =>
        url.includes("/photos/assets/query") && JSON.parse(init!.body as string).facets?.length,
    ).length;
  expect(listings()).toBe(1);
  await waitFor(() => expect(counts()).toBe(1));
  const historyLength = history.length;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(history.length).toBe(historyLength);
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  expect(history.length).toBe(historyLength);
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(location.pathname).toBe("/");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await screen.findByText("1 selected photo");
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(listings()).toBe(2);
  const returnedCache = stored.open.mock.calls.at(-1)![0];
  await waitFor(() => expect(stored.data.get(returnedCache)?.size).toBe(1));
  history.replaceState(null, "", "/");
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await waitFor(() => expect(stored.data.has(returnedCache)).toBe(false));
  changed = true;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await screen.findByText("Your photo library is empty");
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
});

it.each([false, true])(
  "Hidden trash invalidates Documents and Trash restore refreshes only an unlocked grid, locked=%s",
  async (locked) => {
    history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const root = {
      id: 1,
      name: "",
      kind: "dir",
      size: 0,
      revision: 1,
      path: "/",
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
    };
    const node = {
      ...root,
      id: 2,
      parent_id: 1,
      name: "Photo 1.jpg",
      kind: "file",
      mime_type: "image/jpeg",
      current_version_id: "version-1",
      blob_hash: "a".repeat(64),
      photo_asset_id: "photo-1",
      photo_file_count: 1,
    };
    let trashed = false;
    let unlocked = true;
    let rootReads = 0;
    let hiddenReads = 0;
    const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
      if (url.endsWith("/photos/hidden/lock")) {
        unlocked = false;
        return Response.json({});
      }
      if (url.endsWith("/photos/hidden"))
        return Response.json({
          configured: true,
          ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}),
        });
      if (url.includes("/assets/query")) {
        const request = JSON.parse(String(options?.body));
        if (request.facets?.length) return Response.json({ items: [], total: 0, facets: [] });
        const hidden = request.hidden;
        if (hidden) hiddenReads++;
        return Response.json({
          items: hidden && !trashed ? [photo(1)] : [],
          total: hidden && !trashed ? 1 : 0,
        });
      }
      if (url.endsWith("/assets/photo-1/trash")) {
        trashed = true;
        return Response.json({ id: "photo-1", revision: 2 });
      }
      if (url.endsWith("/nodes/2/restore")) {
        trashed = false;
        return Response.json({ ...node, revision: 3, path: "/Photo 1.jpg" });
      }
      if (url.startsWith("/api/v1/trash?"))
        return Response.json({
          items: trashed ? [{ ...node, revision: 2, trashed_at: "2026-07-28T12:01:00Z" }] : [],
          total: trashed ? 1 : 0,
          limit: 1000,
          offset: 0,
        });
      if (url.startsWith("/api/v1/path?")) return Response.json(root);
      if (url === "/api/v1/nodes/1") return Response.json(root);
      if (url.startsWith("/api/v1/nodes/1/children")) {
        rootReads++;
        return Response.json({
          directory: root,
          items: trashed ? [] : [node],
          total: trashed ? 0 : 1,
          limit: 1000,
          offset: 0,
        });
      }
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    });
    vi.stubGlobal("fetch", fetcher);
    render(App);
    await screen.findByRole("cell", { name: "Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole(
        "button",
        { name: "Move to trash" },
      ),
    );
    await waitFor(() => expect(rootReads).toBe(2));
    await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    await waitFor(() => expect(screen.queryByRole("cell", { name: "Photo 1.jpg" })).toBeNull());
    await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
    await screen.findByText("No hidden photos");
    if (locked) {
      await fireEvent.click(screen.getByRole("button", { name: "Lock" }));
      await screen.findByRole("button", { name: "Unlock" });
    }
    const before = hiddenReads;
    await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
    await fireEvent.click(
      within(screen.getByRole("dialog", { name: "Restore Photo 1.jpg from trash" })).getByRole(
        "button",
        { name: "Restore" },
      ),
    );
    await screen.findByText("Restored Photo 1.jpg");
    if (locked) {
      expect(screen.getByRole("button", { name: "Unlock" })).toBeTruthy();
      expect(hiddenReads).toBe(before);
    } else {
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
      expect(hiddenReads).toBe(before + 1);
    }
    await fireEvent.click(screen.getByRole("button", { name: "Close recoverable trash" }));
    await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    await screen.findByRole("cell", { name: "Photo 1.jpg" });
  },
);

it.each([false, true])(
  "keeps direct links and history scoped and conceals a mounted viewer when access changes, hidden=%s",
  async (hidden) => {
    const path = hidden ? "/photos/hidden" : "/photos";
    history.replaceState(
      null,
      "",
      `${path}#web_session=synthetic&web_upload_secret=proof&photo=photo-1`,
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    const revoke = vi.fn();
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL() {
          return "blob:hidden-viewer";
        }
        static revokeObjectURL = revoke;
      },
    );
    Object.defineProperty(HTMLImageElement.prototype, "decode", {
      configurable: true,
      value: vi.fn(async () => {}),
    });
    storage();
    let unlocked = true;
    let changeID = "first";
    let unavailable = false;
    let statusReads = 0;
    let listed = true;
    let holdRefresh = false;
    let finishRefresh!: (response: Response) => void;
    const item = photo(1);
    item.previews.grid = { state: "ready", generation_id: "grid" };
    item.previews.fit = { state: "ready", generation_id: "fit" };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, options?: RequestInit) => {
        const metadata = photoMetadataResponse(url, options);
        if (metadata) return metadata;
        if (url.endsWith("/photos/hidden")) {
          statusReads++;
          if (unavailable) throw new Error("Status unavailable");
          return Response.json({
            configured: true,
            change_id: changeID,
            ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}),
          });
        }
        if (url.endsWith("/hidden/lock")) {
          unlocked = false;
          return Response.json({ configured: true });
        }
        if (url.endsWith("/assets/query")) {
          expect(JSON.parse(String(options?.body)).hidden).toBe(hidden);
          if (holdRefresh) {
            holdRefresh = false;
            return new Promise<Response>((resolve) => (finishRefresh = resolve));
          }
          return Response.json(listed ? { items: [item], total: 1 } : { items: [], total: 0 });
        }
        if (url.includes("/previews/")) return new Response("synthetic-jpeg");
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    await screen.findByText("Fit preview");
    history.pushState(null, "", path);
    await fireEvent(window, new PopStateEvent("popstate"));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
    await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
    expect(location.pathname).toBe(path);
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    history.back();
    await waitFor(() => expect(location.hash).toBe(""));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
    history.forward();
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    const before = revoke.mock.calls.length;
    if (hidden) {
      await fireEvent.click(screen.getByRole("button", { name: "Lock" }));
      await screen.findByRole("button", { name: "Unlock" });
    } else {
      unavailable = true;
      await failStatusReads(() => statusReads, 3);
      await screen.findByText("Status unavailable");
      expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
      expect(location.hash).toBe("#photo=photo-1");
      unavailable = false;
      holdRefresh = true;
      await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
      await waitFor(() => expect(finishRefresh).toBeDefined());
      expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
      finishRefresh(Response.json({ items: [item], total: 1 }));
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
      const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
      await fireEvent(document, new Event("visibilitychange"));
      expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
      await waitFor(() => expect(location.hash).toBe(""));
      visibility.mockReturnValue("visible");
      await fireEvent(document, new Event("visibilitychange"));
      await screen.findByRole("button", { name: "Open Photo 1.jpg" });
      expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
      expect(location.hash).toBe("");
      await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
      const queries = () =>
        vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith("/assets/query")).length;
      let refreshed = queries();
      changeID = "second";
      await fireEvent(document, new Event("visibilitychange"));
      await waitFor(() => expect(queries()).toBeGreaterThan(refreshed), { timeout: 5000 });
      await screen.findByText("Fit preview");
      expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeTruthy();
      expect(location.hash).toBe("#photo=photo-1");
      const shown = revoke.mock.calls.length;
      refreshed = queries();
      listed = false;
      changeID = "third";
      await fireEvent(document, new Event("visibilitychange"));
      await waitFor(() => expect(queries()).toBeGreaterThan(refreshed), { timeout: 5000 });
      await screen.findByText("This photo is no longer in the refreshed result.");
      expect(screen.queryByText("Fit preview")).toBeNull();
      expect(revoke.mock.calls.length).toBeGreaterThan(shown);
      return;
    }
    expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
    await waitFor(() => expect(location.hash).toBe(""));
    expect(history.state?.photoReturn).toBeUndefined();
    expect(revoke.mock.calls.length).toBeGreaterThan(before);
  },
);

it.each([
  "change",
  "return",
  "return before first hide",
  "interrupted return",
  "interrupted return before first hide",
  "direct return",
  "direct album return",
  "direct return before first hide",
  "direct album return before first hide",
])("mounts a fresh scoped Map progressively and revalidates it after %s", async (cause) => {
  const album = photoAlbum();
  const direct = cause.startsWith("direct");
  history.replaceState(
    null,
    "",
    `${direct ? (cause.includes("album") ? `/photos/map/${album.id}` : "/photos/map") : "/photos"}#web_session=synthetic&web_upload_secret=proof`,
  );
  storage();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify({
    tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
    host: "tiles.example.org",
    attribution: "Example",
    max_zoom: 19,
  });
  document.head.append(meta);
  let finish!: (value: Response) => void,
    changed = false,
    reload = false,
    pages = 0;
  let poll!: () => void;
  const interval = globalThis.setInterval;
  vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
    if (delay === 2000) {
      poll = callback as () => void;
      return 0 as unknown as ReturnType<typeof setInterval>;
    }
    return interval(callback, delay, ...args);
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/hidden"))
        return Response.json({
          configured: true,
          change_id: changed ? "second" : cause.includes("before first hide") ? undefined : "first",
        });
      if (url.endsWith("/photos/albums")) return albumResponse([album]);
      if (url.endsWith("/photos/assets/query")) {
        const request = JSON.parse(String(init.body));
        if (!request.projection) return Response.json({ items: [photo(1)], total: 1 });
        pages++;
        if (reload || request.cursor) return new Promise<Response>((resolve) => (finish = resolve));
        return Response.json({
          items: [],
          map_items: [mapPhoto(1)],
          total: 2,
          next_cursor: "second",
        });
      }
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    }),
  );
  render(App);
  if (!direct) {
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: "Map" }));
  }
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  await waitFor(() => expect(map.props?.items.map((row) => row.asset_id)).toEqual(["photo-1"]));
  finish(Response.json({ items: [], map_items: [mapPhoto(2)], total: 2 }));
  await waitFor(() => expect(map.props?.items).toHaveLength(2));
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  async function returnToMap() {
    visibility.mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
  }
  const previous = finish;
  reload = true;
  if (cause.startsWith("return") || direct) {
    const before = pages;
    await returnToMap();
    await screen.findByRole("heading", { name: "Photos in view" });
    expect(pages).toBe(before);
    await returnToMap();
    await screen.findByRole("heading", { name: "Photos in view" });
    expect(pages).toBe(before);
    return;
  }
  if (cause === "change") {
    changed = true;
    await returnToMap();
  } else await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  await waitFor(() => expect(finish).not.toBe(previous));
  if (cause.startsWith("interrupted return")) {
    const interrupted = finish;
    visibility.mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    await act(() => interrupted(Response.json({ items: [], map_items: [mapPhoto(1)], total: 1 })));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => {
      poll();
      expect(finish).not.toBe(interrupted);
    });
  }
  expect(screen.queryByRole("heading", { name: "Photos in view" })).toBeNull();
  finish(Response.json({ detail: "Map unavailable" }, { status: 503 }));
  await screen.findByText("Map unavailable");
  expect(screen.queryByRole("heading", { name: "Photos in view" })).toBeNull();
  const failed = finish;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(finish).not.toBe(failed));
  finish(Response.json({ items: [], map_items: [mapPhoto(2)], total: 1 }));
  await waitFor(() => expect(map.props?.items.map((row) => row.asset_id)).toEqual(["photo-2"]));
});

it.each(["retry", "poll", "changed authority"])(
  "retains staged Map pages after a transient access refresh failure, %s",
  async (recovery) => {
    history.replaceState(null, "", "/photos/map#web_session=synthetic&web_upload_secret=proof");
    storage();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const meta = document.createElement("meta");
    meta.name = "docbank-map-tiles";
    meta.content = JSON.stringify({
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    });
    document.head.append(meta);
    let changeID = "first",
      failed = false,
      poll!: () => void;
    const cursors: (string | undefined)[] = [];
    const interval = globalThis.setInterval;
    vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
      if (delay === 2000) {
        poll = callback as () => void;
        return 0 as unknown as ReturnType<typeof setInterval>;
      }
      return interval(callback, delay, ...args);
    });
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/hidden"))
          return Response.json({ configured: true, change_id: changeID });
        if (url.endsWith("/photos/assets/query")) {
          const request = JSON.parse(String(init.body));
          if (!request.projection) return Response.json({ items: [], total: 0 });
          cursors.push(request.cursor);
          if (changeID === "first")
            return Response.json({ items: [], map_items: [mapPhoto(1)], total: 1 });
          if (request.cursor === "second" && !failed) {
            failed = true;
            return Response.json({ detail: "Map unavailable" }, { status: 503 });
          }
          return Response.json({
            items: [],
            map_items: [mapPhoto(request.cursor ? 3 : 2)],
            total: 2,
            next_cursor: request.cursor ? undefined : "second",
          });
        }
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await waitFor(() => expect(map.props?.items.map((row) => row.asset_id)).toEqual(["photo-1"]));
    changeID = "second";
    await act(() => poll());
    await screen.findByText("Map unavailable");
    expect(screen.queryByRole("heading", { name: "Photos in view" })).toBeNull();
    expect(cursors).toEqual([undefined, undefined, "second"]);
    if (recovery === "changed authority") changeID = "third";
    if (recovery === "retry") await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    else await act(() => poll());
    await screen.findByRole("heading", { name: "Photos in view" });
    expect(map.props?.items.map((row) => row.asset_id)).toEqual(["photo-2", "photo-3"]);
    expect(cursors).toEqual(
      recovery === "changed authority"
        ? [undefined, undefined, "second", undefined, "second"]
        : [undefined, undefined, "second", "second"],
    );
  },
);

it("re-pages Library on return with an unchanged hidden change ID", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let pages = 0,
    finish!: (value: Response) => void;
  const interval = globalThis.setInterval;
  vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) =>
    delay === 2000
      ? (0 as unknown as ReturnType<typeof setInterval>)
      : interval(callback, delay, ...args),
  );
  stubPhotoFetch(
    vi.fn(async (url: string) => {
      if (url.endsWith("/photos/hidden"))
        return Response.json({ configured: true, change_id: "first" });
      if (url.endsWith("/photos/assets/query")) {
        if (++pages > 1) return new Promise<Response>((resolve) => (finish = resolve));
        return Response.json({ items: [photo(1)], total: 1 });
      }
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    }),
  );
  render(App);
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  const visibility = vi.spyOn(document, "visibilityState", "get");
  for (let expected = 2; expected <= 3; expected++) {
    visibility.mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(pages).toBe(expected));
    expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
    finish(Response.json({ items: [photo(1)], total: 1 }));
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  }
});

it("shows Album not found for an unknown album Map", async () => {
  const album = photoAlbum();
  history.replaceState(
    null,
    "",
    `/photos/map/${album.id}#web_session=synthetic&web_upload_secret=proof`,
  );
  storage();
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify({
    tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
    host: "tiles.example.org",
    attribution: "Example",
    max_zoom: 19,
  });
  document.head.append(meta);
  stubPhotoFetch(
    vi.fn(async (url: string) => {
      if (url.endsWith("/photos/albums")) return albumResponse([]);
      if (url.endsWith("/photos/hidden")) return albumResponse({ configured: true });
      return albumResponse({
        items: [],
        map_items: [],
        nodes: [],
        tags: [],
        profiles: [],
        total: 0,
      });
    }),
  );
  render(App);
  await screen.findByText("Album not found");
  expect(screen.queryByText("No located photos")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Back to albums" }));
  expect(location.pathname).toBe("/photos/albums");
});

it.each(["album", "album year", "direct album", "Hidden", "existing gps_bounds"])(
  "keeps Map scoped when opened from %s",
  async (source) => {
    const album = photoAlbum();
    const mapPath = `/photos/map/${album.id}`;
    const libraryScope = source === "Hidden" || source === "existing gps_bounds";
    const gpsBounds =
      source === "existing gps_bounds"
        ? { south: "-1", west: "10", north: "1", east: "15" }
        : { south: "-90", west: "-180", north: "90", east: "180" };
    history.replaceState(
      null,
      "",
      `${source === "direct album" ? mapPath : "/photos"}?retained=library#web_session=synthetic&web_upload_secret=proof`,
    );
    storage();
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const meta = document.createElement("meta");
    meta.name = "docbank-map-tiles";
    meta.content = JSON.stringify({
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    });
    document.head.append(meta);
    const queries: { projection?: string; hidden: boolean; query: Query }[] = [];
    let library: Photos | undefined;
    let albumPhotos: Photos | undefined;
    const resume = Photos.prototype.resume;
    vi.spyOn(Photos.prototype, "resume").mockImplementation(function (this: Photos, ...args) {
      if (!this.projection && !this.hidden && !this.query.filters?.set_ids) library = this;
      if (!this.projection && this.query.filters?.set_ids) albumPhotos = this;
      return resume.apply(this, args);
    });
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/albums")) return albumResponse([album]);
        if (url.endsWith("/photos/hidden"))
          return albumResponse({
            configured: true,
            expires_at: "2099-01-01T00:00:00Z",
            expires_in: 300,
          });
        if (url.endsWith("/photos/assets/query")) {
          const request = JSON.parse(String(init.body));
          queries.push(request);
          return albumResponse(
            request.projection
              ? { items: [], map_items: [mapPhoto(1)], total: 1 }
              : { items: [photo(1)], total: 1 },
          );
        }
        return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    if (source !== "direct album") {
      await screen.findByRole("button", { name: "Open Photo 1.jpg" });
      library!.baseQuery = {
        ...library!.baseQuery,
        text: "beach",
        sort: { field: "relevance", direction: "desc" },
        filters: {
          cameras: ["Library camera"],
          ...(source === "existing gps_bounds" ? { gps_bounds: gpsBounds } : {}),
        },
      };
      await library!.selectDate("2024");
      if (source !== "existing gps_bounds") {
        await fireEvent.click(
          screen.getByRole("button", { name: source.startsWith("album") ? /^Trip/ : "Hidden" }),
        );
        await screen.findByRole("heading", {
          name: source.startsWith("album") ? "Trip" : "Hidden",
        });
        if (source === "album year") {
          await fireEvent.click(screen.getByRole("button", { name: "Timeline" }));
          await albumPhotos!.selectDate("2023");
        }
      }
      if (source === "Hidden") {
        await waitFor(() =>
          expect(
            (screen.getByRole("button", { name: "Search" }) as HTMLButtonElement).disabled,
          ).toBe(false),
        );
        await fireEvent.input(await screen.findByRole("searchbox", { name: "Search photos" }), {
          target: { value: "private" },
        });
        await fireEvent.click(screen.getByRole("button", { name: "Search" }));
        await waitFor(() =>
          expect(
            queries.some((request) => request.hidden && request.query.text === "private"),
          ).toBe(true),
        );
      }
      await fireEvent.click(screen.getByRole("button", { name: "Map" }));
    }
    await waitFor(() => expect(queries.some((request) => request.projection === "map")).toBe(true));
    const request = queries.find((request) => request.projection === "map")!;
    expect(location.pathname).toBe(libraryScope ? "/photos/map" : mapPath);
    expect(location.search).toBe("?retained=library");
    expect(
      within(screen.getByRole("navigation", { name: "Docbank navigation" }))
        .getByRole("button", { name: "Map" })
        .getAttribute("aria-current"),
    ).toBe("page");
    expect(request.hidden).toBe(false);
    expect(request.query.sort).toEqual({ field: "capture_time", direction: "desc" });
    expect(request.query.filters.gps_bounds).toEqual(gpsBounds);
    if (libraryScope) {
      expect(request.query).toMatchObject({
        text: "beach",
        filters: {
          cameras: ["Library camera"],
          capture_after: "2024-01-01",
          capture_before: "2025-01-01",
        },
      });
      expect(request.query.filters.set_ids).toBeUndefined();
    } else {
      expect(request.query.text).toBe("");
      expect(request.query.filters.set_ids).toEqual([album.id]);
      expect(request.query.filters.cameras).toBeUndefined();
      expect(request.query.filters.capture_after).toBe(
        source === "album year" ? "2023-01-01" : undefined,
      );
      expect(request.query.filters.capture_before).toBe(
        source === "album year" ? "2024-01-01" : undefined,
      );
    }
    await waitFor(() => expect(map.props?.items).toHaveLength(1));
    const reads = queries.length;
    await fireEvent.click(
      within(screen.getByRole("navigation", { name: "Docbank navigation" })).getByRole("button", {
        name: "Map",
      }),
    );
    expect(location.pathname).toBe(libraryScope ? "/photos/map" : mapPath);
    expect(queries).toHaveLength(reads);
  },
);

it.each(["Library", "album"])(
  "closes the %s viewer from its active sidebar entry without reloading",
  async (kind) => {
    const album = photoAlbum({
      id: "22222222-2222-4222-8222-0000000000aa",
      included_count: 1,
      member_count: 1,
    });
    const path = kind === "album" ? `/photos/albums/${album.id}` : "/photos";
    history.replaceState(null, "", `${path}#web_session=synthetic&web_upload_secret=proof`);
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
    Element.prototype.scrollIntoView = vi.fn();
    const queries: { filters?: { set_ids?: string[] } }[] = [];
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
        if (url.endsWith("/photos/assets/query")) {
          queries.push(JSON.parse(init.body as string).query);
          return new Response(JSON.stringify({ items: [photo(1)], total: 1 }));
        }
        if (url.includes("/nodes/1"))
          return new Response(
            JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }),
          );
        return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
      }),
    );
    render(App);
    await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), {
      key: "Enter",
    });
    await screen.findByRole("dialog", { name: /Photo viewer/ });
    expect(location.pathname).toBe(path);
    expect(new URLSearchParams(location.hash.slice(1)).get("photo")).toBe("photo-1");
    expect(
      screen.getByRole("main", { name: kind === "album" ? "Photo album" : "Photo library" }),
    ).toBeTruthy();
    expect(queries[0]?.filters?.set_ids).toEqual(kind === "album" ? [album.id] : undefined);
    const reads = queries.length;
    await fireEvent.click(
      screen.getByRole("button", { name: kind === "album" ? /^Trip/ : "Library" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
    await waitFor(() => expect(location.hash).not.toContain("photo="));
    expect(queries).toHaveLength(reads);
    expect(location.pathname).toBe(path);
    await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), {
      key: "Enter",
    });
    await screen.findByRole("dialog", { name: /Photo viewer/ });
    await fireEvent.click(
      screen.getByRole("button", { name: kind === "Library" ? "Close photo viewer" : "Library" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
    expect(location.pathname).toBe("/photos");
    await waitFor(() => expect(location.hash).not.toContain("photo="));
  },
);

it.each([undefined, "first"])(
  "conceals an open album when hidden photos change, then rereads its photos and album counts, initial change=%s",
  async (initial) => {
    const album = photoAlbum({
      id: "22222222-2222-4222-8222-0000000000ab",
      included_count: 1,
      member_count: 1,
    });
    history.replaceState(
      null,
      "",
      `/photos/albums/${album.id}#web_session=synthetic&web_upload_secret=proof`,
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    storage();
    let changed = false;
    let albumReads = 0;
    stubPhotoFetch(
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/photos/hidden"))
          return Response.json({ configured: true, change_id: changed ? "second" : initial });
        if (url.endsWith("/photos/albums")) {
          albumReads++;
          return Response.json([{ ...album, included_count: changed ? 0 : 1 }]);
        }
        if (url.endsWith("/photos/assets/query")) {
          expect(JSON.parse(init.body as string).query.filters.set_ids).toEqual([album.id]);
          return Response.json({ items: changed ? [] : [photo(1)], total: changed ? 0 : 1 });
        }
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    expect(screen.getByRole("main", { name: "Photo album" })).toBeTruthy();
    const reads = albumReads;
    changed = true;
    await waitFor(
      () => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull(),
      { timeout: 5000 },
    );
    await screen.findByText("This album is empty", undefined, { timeout: 5000 });
    expect(albumReads).toBeGreaterThan(reads);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^Trip/ }).textContent).toContain("0"),
    );
    expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  },
);

it("restores photo scroll while resumed counts are pending and keeps subsequent scrolling", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  const counts: { signal: AbortSignal; finish: (response: Response) => void }[] = [];
  let listings = 0;
  let finishListing: ((response: Response) => void) | undefined;
  vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
    if (url.includes("/photos/assets/query")) {
      const request = JSON.parse(init!.body as string);
      if (request.facets?.length)
        return new Promise<Response>((finish) => counts.push({ signal: init!.signal!, finish }));
      listings++;
      if (listings === 2) return new Promise<Response>((finish) => (finishListing = finish));
      return Response.json({
        items: Array.from({ length: 100 }, (_, index) => photo(index + 1)),
        total: 100,
      });
    }
    if (url.endsWith("/photos/hidden"))
      return Response.json({ configured: true, change_id: "first" });
    if (url.includes("/nodes/1"))
      return Response.json({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await waitFor(() => expect(counts).toHaveLength(1));
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  await act(async () => {});
  const original = screen.getByTestId("photo-scroll");
  original.scrollTop = 480;
  await fireEvent.scroll(original);
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(counts[0].signal.aborted).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await waitFor(() => expect(finishListing).toBeTypeOf("function"));
  await act(async () =>
    finishListing!(
      Response.json({
        items: Array.from({ length: 100 }, (_, index) => photo(index + 1)),
        total: 100,
      }),
    ),
  );
  const resumed = await screen.findByTestId("photo-scroll");
  await waitFor(() => expect(counts).toHaveLength(2));
  await waitFor(() => expect(resumed.scrollTop).toBe(480));
  // Leaving Photos conceals the Library, so returning revalidates it with one refresh.
  expect(listings).toBe(2);
  resumed.scrollTop = 960;
  await fireEvent.scroll(resumed);
  await act(async () => {
    counts[0].finish(Response.json({ items: [], total: 100, facets: [] }));
    counts[1].finish(Response.json({ items: [], total: 100, facets: [] }));
  });
  await waitFor(() => expect(screen.queryByText(/Loading counts/)).toBeNull());
  await act(async () => {});
  expect(resumed.scrollTop).toBe(960);
});

it("refreshes Documents, Library, and album counts after adding photos", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  vi.spyOn(VerifiedUploadChannel.prototype, "connect").mockResolvedValue();
  vi.spyOn(VerifiedUploadChannel.prototype, "planPhotoImport").mockResolvedValue({
    import_id: "import-1",
    groups: [[0]],
    unsupported: [],
  });
  let imported = false;
  vi.spyOn(VerifiedUploadChannel.prototype, "uploadPhotoGroup").mockImplementation(async () => {
    imported = true;
    return { status: "added", asset_id: "photo-1" };
  });
  const root = { id: 1, kind: "dir", name: "", revision: 1, path: "/" };
  const file = {
    id: 3,
    parent_id: 1,
    kind: "file",
    name: "capture.JPG",
    revision: 1,
    path: "/capture.JPG",
    mime_type: "image/jpeg",
    size: 4,
    current_version_id: "11111111-1111-4111-8111-111111111111",
    blob_hash: "a".repeat(64),
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
  };
  const reads = { documents: 0, albums: 0, library: 0 };
  stubPhotoFetch(async (url: string) => {
    if (url.endsWith("/photos/albums")) {
      reads.albums++;
      return albumResponse([photoAlbum({ included_count: imported ? 1 : 0 })]);
    }
    if (url.endsWith("/photos/assets/query")) {
      reads.library++;
      return albumResponse({ items: imported ? [photo(1)] : [], total: imported ? 1 : 0 });
    }
    if (url === "/api/v1/path?path=%2F") return albumResponse(root);
    if (url.includes("/nodes/1/children")) {
      reads.documents++;
      return albumResponse({
        directory: root,
        items: imported ? [file] : [],
        total: imported ? 1 : 0,
      });
    }
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await screen.findByText("Your photo library is empty");
  await waitFor(() => expect(reads.documents).toBe(1));
  const before = { ...reads };
  await fireEvent.click(screen.getAllByRole("button", { name: "Add photos" })[0]);
  await fireEvent.change(await screen.findByLabelText("Choose photo files"), {
    target: { files: [new File(["jpeg"], "capture.JPG")] },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
  await waitFor(() => {
    for (const key of Object.keys(reads) as (keyof typeof reads)[])
      expect(reads[key]).toBeGreaterThan(before[key]);
  });
  await fireEvent.click(screen.getByRole("button", { name: "Close add photos" }));
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  await screen.findByRole("cell", { name: "capture.JPG" });
});

it.each(["search", "tag", "snapshot"] as const)(
  "retains the Documents %s after adding photos",
  async (mode) => {
    const query = parseQuery("{}");
    const fingerprint = await queryFingerprint(query);
    history.replaceState(
      null,
      "",
      `/#web_session=synthetic&web_upload_secret=proof${mode === "snapshot" ? `&query=${encodeURIComponent(JSON.stringify(query))}` : ""}`,
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
    Element.prototype.scrollIntoView = vi.fn();
    vi.spyOn(VerifiedUploadChannel.prototype, "connect").mockResolvedValue();
    vi.spyOn(VerifiedUploadChannel.prototype, "planPhotoImport").mockResolvedValue({
      import_id: "import-1",
      groups: [[0]],
      unsupported: [],
    });
    const upload = vi
      .spyOn(VerifiedUploadChannel.prototype, "uploadPhotoGroup")
      .mockResolvedValue({ status: "added", asset_id: "photo-1" });
    const root = { id: 1, kind: "dir", name: "", revision: 1, path: "/" };
    const tag = {
      id: "33333333-3333-4333-8333-333333333333",
      name: "reviewed",
      revision: 1,
      assignment_count: 1,
    };
    const reads = { directory: 0, search: 0, tag: 0, snapshot: 0, photos: 0 };
    stubPhotoFetch(async (url: string, init: RequestInit) => {
      if (url.endsWith("/processing/profiles")) return albumResponse([]);
      if (url.endsWith("/photos/assets/query")) {
        reads.photos++;
        return albumResponse({ items: [], total: 0 });
      }
      if (url === "/api/v1/path?path=%2F") return albumResponse(root);
      if (url.includes("/nodes/1/children")) {
        reads.directory++;
        return albumResponse({ directory: root, items: [], total: 0 });
      }
      if (url.startsWith("/api/v1/search?")) {
        reads.search++;
        return albumResponse({ hits: [], limit: 1000, truncated: false });
      }
      if (url.includes(`/tags/${tag.id}/nodes`)) {
        reads.tag++;
        return albumResponse({ items: [], total: 0, omitted_trashed: 0 });
      }
      if (url.startsWith("/api/v1/tags?")) return albumResponse({ items: [tag], total: 1 });
      if (url.endsWith("/queries/parse"))
        return albumResponse({ query, query_fingerprint: fingerprint, dependencies: [] });
      if (url.endsWith("/workspace/queries")) {
        reads.snapshot++;
        return albumResponse({
          query,
          dependencies: [],
          query_fingerprint: fingerprint,
          member_hash: "a".repeat(64),
          snapshot_fingerprint: `sha256:${"b".repeat(64)}`,
          generation: { kind: "native" },
          coverage: { configuration: "unconfigured" },
          observed_at: "2026-10-14T12:00:00Z",
          page_size: 100,
          total: 0,
          total_bytes: 0,
          rows: [],
          facets: JSON.parse(String(init.body)).facets.map((dimension: string) => ({
            dimension,
            available: false,
            reason: "not_requested",
            values: [],
          })),
          snapshot: true,
          snapshot_id: "a".repeat(32),
          created_at: "2026-10-14T12:00:00Z",
          expires_at: "2026-10-14T12:30:00Z",
        });
      }
      return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
    });
    render(App);
    await waitFor(() => expect(reads.directory).toBe(1));
    if (mode === "search") {
      const input = await screen.findByRole("searchbox", { name: "Search documents" });
      await fireEvent.input(input, { target: { value: "retained" } });
      await fireEvent.submit(input.closest("form")!);
      await screen.findByText("Search results");
    } else if (mode === "tag") {
      const filter = await screen.findByRole("combobox", {
        name: "Browse or filter by tag: All tags",
      });
      await waitFor(() => expect(filter.hasAttribute("disabled")).toBe(false));
      await fireEvent.click(filter);
      await fireEvent.click(await screen.findByRole("option", { name: "reviewed (1)" }));
      await screen.findByText("Documents tagged");
    } else {
      await fireEvent.click(await screen.findByRole("button", { name: "Run query" }));
      await screen.findByRole("region", { name: "Frozen query results" });
    }
    const before = { ...reads };
    await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    await fireEvent.click((await screen.findAllByRole("button", { name: "Add photos" }))[0]);
    await fireEvent.change(await screen.findByLabelText("Choose photo files"), {
      target: { files: [new File(["jpeg"], "capture.JPG")] },
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
    await waitFor(() => expect(upload).toHaveBeenCalledOnce());
    await waitFor(() => expect(reads.photos).toBeGreaterThan(before.photos + 1));
    await fireEvent.click(screen.getByRole("button", { name: "Close add photos" }));
    await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    expect(reads.directory).toBe(before.directory);
    expect(reads[mode]).toBe(before[mode]);
    if (mode === "search") {
      expect(screen.getByText("Search results")).toBeTruthy();
      expect(
        (screen.getByRole("searchbox", { name: "Search documents" }) as HTMLInputElement).value,
      ).toBe("retained");
    } else if (mode === "tag") expect(screen.getByText("Documents tagged")).toBeTruthy();
    else
      expect(
        screen
          .getByRole("region", { name: "Frozen query results" })
          .getAttribute("data-snapshot-id"),
      ).toBe("a".repeat(32));
  },
);

it("opens a smart set through the Albums app route", async () => {
  history.replaceState(null, "", "/photos/albums#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const payload = parseQuery(
    '{"syntax":"advanced","text":"rating:5","filters":{"kinds":["photo"]},"sort":{"field":"size","direction":"desc"}}',
  );
  const saved = savedSmartSet(payload, await queryFingerprint(payload), { revision: 2 });
  const open = vi.spyOn(Photos.prototype, "openSmartSet").mockReturnValue(true);
  stubPhotoFetch(
    vi.fn(async (url: string) => {
      if (url.includes("/saved-queries?"))
        return Response.json({ items: [saved], total: 1, limit: 100, offset: 0 });
      if (url.endsWith("/photos/albums")) return albumResponse([]);
      if (url.endsWith("/photos/assets/query"))
        return albumResponse({ items: [photo(1)], total: 1 });
      if (url.includes("/nodes/1"))
        return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
      return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
    }),
  );
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Five stars" }));
  await waitFor(() => expect(location.pathname).toBe("/photos"));
  expect(open).toHaveBeenCalledWith(saved);
});

it("passes a registered Photos shortcut into the help dialog", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  stubPhotoFetch(async (url) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: false });
    if (url.endsWith("/photos/assets/query"))
      return Response.json({ items: [photo(1)], total: 1, facets: [] });
    return Response.json({ items: [], total: 0 });
  });
  render(App);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  const edit = vi.spyOn(PhotoAuthoredEditor.prototype, "edit").mockResolvedValue(undefined);
  await fireEvent.click(screen.getByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.keyDown(window, { key: "&", code: "Digit6" });
  await waitFor(() =>
    expect(edit).toHaveBeenCalledWith(
      { asset_ids: ["photo-1"] },
      { label: "red" },
      false,
      expect.any(Function),
      undefined,
      undefined,
    ),
  );
  await fireEvent.keyDown(window, { key: "r" });
  await waitFor(() =>
    expect(edit).toHaveBeenCalledWith(
      { asset_ids: ["photo-1"] },
      {},
      false,
      expect.any(Function),
      undefined,
      90,
    ),
  );
  document.body.focus();
  await fireEvent.keyDown(window, { key: "?", shiftKey: true });
  const help = await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
  expect(within(help).getByText("Clear rating")).toBeTruthy();
  expect(within(help).getByText("Set purple label")).toBeTruthy();
  expect(within(help).getByText("Rotate selected photos 90° clockwise")).toBeTruthy();
  expect(within(help).getByText("Add selected photos to album")).toBeTruthy();
});

it("refuses draft navigation until Clear edits releases it", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("navigation", undefined);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Element.prototype.scrollIntoView = vi.fn();
  const root = { id: 1, kind: "dir", name: "", revision: 1, path: "/" };
  stubPhotoFetch(async (url) => {
    if (url.endsWith("/photos/assets/query"))
      return Response.json({ items: [photo(1), photo(2)], total: 2 });
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: false });
    if (url.startsWith("/api/v1/path?") || url === "/api/v1/nodes/1") return Response.json(root);
    if (url.startsWith("/api/v1/nodes/1/children"))
      return Response.json({ directory: root, items: [], total: 0 });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Photos" }));
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(within(viewer).getByRole("button", { name: "Metadata" }));
  const caption = await within(viewer).findByRole("textbox", { name: "Caption" });
  await waitFor(() => expect(caption.hasAttribute("disabled")).toBe(false));
  await fireEvent.input(caption, { target: { value: "Keep my caption" } });
  const unloadBlocked = () => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  expect(unloadBlocked()).toBe(true);
  history.back();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect((screen.getByRole("textbox", { name: "Caption" }) as HTMLTextAreaElement).value).toBe(
    "Keep my caption",
  );
  await fireEvent.click(within(viewer).getByRole("button", { name: "Clear edits" }));
  await waitFor(() =>
    expect(screen.getByRole("textbox", { name: "Caption" }).hasAttribute("disabled")).toBe(false),
  );
  expect(unloadBlocked()).toBe(false);
});

it("closes the viewer to the grid after refusing a typed photo hash", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("navigation", undefined);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Element.prototype.scrollIntoView = vi.fn();
  stubPhotoFetch(async (url) => {
    if (url.endsWith("/photos/assets/query"))
      return Response.json({ items: [photo(1), photo(2)], total: 2 });
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: false });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(within(viewer).getByRole("button", { name: "Metadata" }));
  const caption = await within(viewer).findByRole("textbox", { name: "Caption" });
  await waitFor(() => expect(caption.hasAttribute("disabled")).toBe(false));
  await fireEvent.input(caption, { target: { value: "Draft caption" } });
  const typed = new URL(location.href);
  typed.hash = "photo=photo-2";
  history.pushState(null, "", typed.href);
  window.dispatchEvent(new PopStateEvent("popstate"));
  window.dispatchEvent(new HashChangeEvent("hashchange"));
  await waitFor(() => expect(location.hash).toContain("photo=photo-1"));
  const kept = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(await within(kept).findByRole("button", { name: "Clear edits" }));
  await fireEvent.click(within(kept).getByRole("button", { name: "Close photo viewer" }));
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeNull(),
  );
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(screen.queryByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeNull();
  expect(location.hash).not.toContain("photo=");
});

async function applyAlbumKeyword() {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ member_count: 1, included_count: 1 });
  let coverRotation = 0;
  const albumRotations: number[] = [];
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    path: "/",
    created_at: "2026-01-01T00:00:00Z",
    modified_at: "2026-01-01T00:00:00Z",
  };
  const document = () => ({
    ...root,
    id: 1,
    parent_id: root.id,
    name: "Photo 1.jpg",
    kind: "file",
    revision: documentRevision,
    size: 74,
    mime_type: "image/jpeg",
    current_version_id: "version-1",
    blob_hash: "a".repeat(64),
    path: "/Photo 1.jpg",
  });
  const tag = {
    id: "22222222-2222-4222-8222-222222222222",
    name: "Portfolio",
    revision: 1,
    assignment_count: 1,
  };
  const before = preparedPhoto(1);
  const rotated = { ...before, revision: 2, values: { ...before.values, rotation: 90 as const } };
  const edited = { ...rotated, revision: 3 };
  let documentRevision = 1;
  let keywordApplied = false;
  let editCount = 0;
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/photos/albums")) {
      albumRotations.push(coverRotation);
      return albumResponse([{ ...album, cover_rotation: coverRotation }]);
    }
    if (url.endsWith("/photos/assets/query")) return albumResponse({ items: [photo(1)], total: 1 });
    if (url === "/api/v1/path?path=%2F") return albumResponse(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
      return albumResponse({
        directory: root,
        items: [document()],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/nodes/1") return albumResponse(document());
    if (url === "/api/v1/nodes/1/tags?limit=1000&offset=0")
      return albumResponse({
        items: keywordApplied ? [tag] : [],
        total: Number(keywordApplied),
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return albumResponse({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === `/api/v1/tags/${tag.id}`)
      return albumResponse({
        ...tag,
        revision: documentRevision,
        assignment_count: keywordApplied ? 2 : 1,
      });
    if (url.endsWith("/photos/authored/edit")) {
      if (editCount++ === 0) {
        coverRotation = 90;
        documentRevision = 2;
        return albumResponse({ receipt_id: "rotated", before: [before], after: [rotated] });
      }
      keywordApplied = true;
      documentRevision = 3;
      return albumResponse({
        receipt_id: "edited",
        before: [rotated],
        after: [edited],
        keywords: [{ node_id: 1, tag_id: tag.id, before: false, after: true }],
        keyword_nodes: [{ node_id: 1, after_revision: 3 }],
      });
    }
    if (url.endsWith("/photos/authored/edited/undo")) {
      keywordApplied = false;
      documentRevision = 4;
      return albumResponse({
        receipt_id: "undone",
        before: [edited],
        after: [{ ...rotated, revision: 4 }],
        keywords: [{ node_id: 1, tag_id: tag.id, before: true, after: false }],
        keyword_nodes: [{ node_id: 1, after_revision: 4 }],
      });
    }
    if (url.startsWith("/api/v1/audit/status"))
      return albumResponse({ enabled: false, scopes: [] });
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  });
  stubPhotoFetch(fetcher);
  const changed = vi.spyOn(Photos.prototype, "metadataChanged");
  render(App);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.keyDown(window, { key: "r" });
  await waitFor(() => expect(albumRotations).toContain(90));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(2));
  await waitFor(() => expect((changed.mock.contexts[1] as Photos).authored.busy).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: /Keyword:/ }).hasAttribute("disabled")).toBe(false),
  );
  await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
  await fireEvent.click(screen.getByRole("option", { name: /Portfolio/ }));
  await fireEvent.click(screen.getByRole("button", { name: "Add keyword" }));
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(4));
  await waitFor(() => expect((changed.mock.contexts[3] as Photos).authored.busy).toBe(false));
  return { changed, album, fetcher, albumRotations };
}

it("forwards photo edit and Undo receipts to Library, Albums, and Documents", async () => {
  const { changed, album, fetcher, albumRotations } = await applyAlbumKeyword();
  const library = changed.mock.contexts[0] as Photos;
  expect(albumRotations).toContain(90);
  expect(library.query.filters?.set_ids).toBeUndefined();
  expect((changed.mock.contexts[1] as Photos).query.filters?.set_ids).toEqual([album.id]);
  expect(
    changed.mock.calls.slice(0, 2).every(([receipt]) => receipt.receipt_id === "rotated"),
  ).toBe(true);
  expect(changed.mock.calls.slice(2, 4).every(([receipt]) => receipt.receipt_id === "edited")).toBe(
    true,
  );
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  await fireEvent.click(await screen.findByRole("cell", { name: "Photo 1.jpg" }));
  const inspector = await screen.findByRole("region", {
    name: "Document authority for Photo 1.jpg",
  });
  await waitFor(() =>
    expect(within(inspector).getByRole("group", { name: "Assigned tags" }).textContent).toContain(
      "Portfolio",
    ),
  );
  expect(within(inspector).getByText("Revision").nextElementSibling?.textContent).toBe("3");
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await screen.findByText("1 photos · 1 loaded");
  fetcher.mockClear();
  await fireEvent.click(await screen.findByRole("button", { name: "Undo last photo edit" }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(6));
  expect(changed.mock.contexts[4]).toBe(library);
  expect(changed.mock.contexts[5]).toBe(changed.mock.contexts[1]);
  expect(changed.mock.calls.slice(4).every(([receipt]) => receipt.receipt_id === "undone")).toBe(
    true,
  );
  await waitFor(() => expect((changed.mock.contexts[3] as Photos).authored.busy).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  const undoneInspector = await screen.findByRole("region", {
    name: "Document authority for Photo 1.jpg",
  });
  await waitFor(() =>
    expect(within(undoneInspector).getByText("No tags are assigned to this node.")).toBeTruthy(),
  );
  expect(fetcher).toHaveBeenCalledWith("/api/v1/nodes/1", expect.anything());
  expect(within(undoneInspector).getByText("Revision").nextElementSibling?.textContent).toBe("4");
});

it("updates unfiltered Library and album rows after edits without reloading them", async () => {
  const refresh = vi.spyOn(Photos.prototype, "refresh");
  const { changed } = await applyAlbumKeyword();
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(4));
  expect(new Set(changed.mock.contexts).size).toBe(2);
  const firstReceipt = changed.mock.invocationCallOrder[0]!;
  expect(refresh.mock.invocationCallOrder.filter((order) => order > firstReceipt)).toEqual([]);
});

it("discards shared photo Undo when signing into a replacement web session", async () => {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  history.replaceState(null, "", "/photos");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  let sessions = 0;
  stubPhotoFetch(async (url) => {
    if (url === "/api/daemon/web-auth") return Response.json({ enabled: true });
    if (url === "/api/daemon/web-auth/login")
      return Response.json({
        enabled: true,
        session: { token: `session-${++sessions}`, upload_secret: "synthetic-proof" },
      });
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: false });
    if (url.endsWith("/photos/assets/query"))
      return Response.json({ items: [photo(1)], total: 1, facets: [] });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  const resumed = vi.spyOn(Photos.prototype, "resume");
  vi.spyOn(PhotoAuthoredEditor.prototype, "edit").mockImplementation(
    async function (this: PhotoAuthoredEditor) {
      this.receiptID = "last-edit";
      this.undoState.summary = "1 file updated: rating";
    },
  );
  render(App);
  const signIn = async () => {
    await fireEvent.input(await screen.findByLabelText("API key"), {
      target: { value: "synthetic-key" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    return screen.findByRole("button", { name: "Select Photo 1.jpg" });
  };
  await fireEvent.click(await signIn());
  await fireEvent.keyDown(window, { key: "4" });
  await screen.findByRole("button", { name: "Undo last photo edit" });
  const previous = (resumed.mock.contexts.at(-1) as Photos).authored.undoState;
  await fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
  await signIn();
  const current = (resumed.mock.contexts.at(-1) as Photos).authored.undoState;
  expect(current).not.toBe(previous);
  expect(current.receiptID).toBe("");
  expect(current.summary).toBe("");
  expect(screen.queryByRole("button", { name: "Undo last photo edit" })).toBeNull();
});

it("loads keywords beyond the first catalog page", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const catalog = Array.from({ length: 1000 }, (_, index) => ({
    id: `tag-${index}`,
    name: `Keyword ${index}`,
    revision: 1,
    assignment_count: 0,
  }));
  const last = { id: "last", name: "Beyond the first page", revision: 1, assignment_count: 0 };
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return Response.json({ items: catalog, total: 1001 });
    if (url === "/api/v1/tags?limit=1000&offset=1000")
      return Response.json({ items: [last], total: 1001 });
    if (url.endsWith("/photos/assets/query")) return Response.json({ items: [photo(1)], total: 1 });
    if (url.includes("/nodes/1"))
      return Response.json({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  stubPhotoFetch(fetcher);
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const inspector = await screen.findByRole("region", { name: "Photo metadata" });
  const more = await within(inspector).findByRole("button", { name: "Load more keywords" });
  await waitFor(() => expect((more as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(more);
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Load more keywords" })).toBeNull(),
  );
  expect(fetcher.mock.calls.some(([url]) => url === "/api/v1/tags?limit=1000&offset=1000")).toBe(
    true,
  );
  await fireEvent.click(
    within(inspector).getByRole("combobox", { name: "Keyword: Choose keyword" }),
  );
  expect(
    await within(inspector).findByRole("option", { name: /Beyond the first page/ }),
  ).toBeTruthy();
});

it("guards a Hidden draft while a status failure conceals its workspace", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal("navigation", undefined);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Element.prototype.scrollIntoView = vi.fn();
  let unavailable = false;
  stubPhotoFetch(async (url) => {
    if (url.endsWith("/photos/hidden"))
      return unavailable
        ? Response.json({ detail: "Status unavailable" }, { status: 503 })
        : Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300 });
    if (url.endsWith("/photos/assets/query")) return Response.json({ items: [photo(1)], total: 1 });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const caption = await screen.findByRole("textbox", { name: "Caption" });
  await waitFor(() => expect(caption.hasAttribute("disabled")).toBe(false));
  await fireEvent.input(caption, { target: { value: "Retained Hidden caption" } });
  unavailable = true;
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Status unavailable");
  expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
  for (const name of ["Documents", "Library"]) {
    await fireEvent.click(screen.getByRole("button", { name }));
    expect(location.pathname).toBe("/photos/hidden");
  }
  unavailable = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  expect(
    ((await screen.findByRole("textbox", { name: "Caption" })) as HTMLInputElement).value,
  ).toBe("Retained Hidden caption");
});

it("keeps a Hidden viewer's return point while a status failure refuses to close its draft", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal("navigation", undefined);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Element.prototype.scrollIntoView = vi.fn();
  let unavailable = false;
  stubPhotoFetch(async (url) => {
    if (url.endsWith("/photos/hidden"))
      return unavailable
        ? Response.json({ detail: "Status unavailable" }, { status: 503 })
        : Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300 });
    if (url.endsWith("/photos/assets/query"))
      return Response.json({ items: [photo(1), photo(2)], total: 2 });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(within(viewer).getByRole("button", { name: "Metadata" }));
  const caption = await within(viewer).findByRole("textbox", { name: "Caption" });
  await waitFor(() => expect(caption.hasAttribute("disabled")).toBe(false));
  await fireEvent.input(caption, { target: { value: "Retained Hidden caption" } });
  unavailable = true;
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Status unavailable");
  unavailable = false;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  const restored = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  if (!within(restored).queryByRole("button", { name: "Clear edits" }))
    await fireEvent.click(within(restored).getByRole("button", { name: "Metadata" }));
  await fireEvent.click(await within(restored).findByRole("button", { name: "Clear edits" }));
  await fireEvent.keyDown(restored, { key: "ArrowRight" });
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 2.jpg" });
});

it.each(["while open", "after a status failure"])(
  "closes a Hidden viewer with a staged draft when Hidden locks %s",
  async (scenario) => {
    history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
    storage();
    vi.stubGlobal("navigation", undefined);
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    Element.prototype.scrollIntoView = vi.fn();
    let locked = false,
      unavailable = false;
    stubPhotoFetch(async (url) => {
      if (url.endsWith("/photos/hidden/lock")) {
        locked = true;
        return Response.json({ configured: true });
      }
      if (url.endsWith("/photos/hidden") && unavailable)
        return Response.json({ detail: "Status unavailable" }, { status: 503 });
      if (url.endsWith("/photos/hidden"))
        return Response.json(
          locked
            ? { configured: true }
            : { configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300 },
        );
      if (url.endsWith("/photos/assets/query"))
        return Response.json({ items: [photo(1)], total: 1 });
      return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
    });
    render(App);
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
    const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    expect(location.hash).toContain("photo=photo-1");
    await fireEvent.click(within(viewer).getByRole("button", { name: "Metadata" }));
    const caption = await within(viewer).findByRole("textbox", { name: "Caption" });
    await waitFor(() => expect(caption.hasAttribute("disabled")).toBe(false));
    await fireEvent.input(caption, { target: { value: "Discarded Hidden caption" } });
    if (scenario === "while open") {
      await fireEvent.click(screen.getByRole("button", { name: "Lock" }));
      await waitFor(() => expect(locked).toBe(true));
    } else {
      unavailable = true;
      await fireEvent(document, new Event("visibilitychange"));
      await screen.findByText("Status unavailable");
      expect(location.hash).toContain("photo=photo-1");
      unavailable = false;
      locked = true;
      await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    }
    await waitFor(() => expect(location.hash).not.toContain("photo="));
    expect(screen.queryByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeNull();
  },
);

it("resume requests Map authored revalidation", async () => {
  history.replaceState(null, "", "/photos/map#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Element.prototype.scrollIntoView = vi.fn();
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify({
    tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
    host: "tiles.example.org",
    attribution: "Example",
    max_zoom: 19,
  });
  document.head.append(meta);
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden"))
      return Response.json({ configured: true, change_id: "same" });
    if (url.endsWith("/photos/assets/query")) {
      return Response.json({ items: [], map_items: [mapPhoto(1)], total: 1, facets: [] });
    }
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  stubPhotoFetch(fetcher);
  render(App);
  await waitFor(() => expect(map.props?.items).toHaveLength(1));
  await act(() => map.props!.onvisible(map.props!.items));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  const revalidate = vi.spyOn(Photos.prototype, "revalidateAuthored");
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  visibility.mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(revalidate).toHaveBeenCalled());
});

it.each([false, true])(
  "keeps a finished photo job through Documents export and prepares the current selection, hidden=%s",
  async (hidden) => {
    history.replaceState(
      null,
      "",
      `${hidden ? "/photos/hidden" : "/photos"}#web_session=synthetic&web_upload_secret=proof`,
    );
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    storage();
    const hash = "a".repeat(64),
      future = "2099-01-01T00:00:00Z";
    const root = {
      id: 1,
      name: "",
      kind: "dir",
      size: 0,
      revision: 1,
      path: "/",
      created_at: "2026-01-01T00:00:00Z",
      modified_at: "2026-01-01T00:00:00Z",
    };
    const document = {
      ...root,
      id: 2,
      parent_id: 1,
      name: "Synthetic document.txt",
      kind: "file",
      mime_type: "text/plain",
      current_version_id: "22222222-2222-4222-8222-222222222222",
      blob_hash: hash,
    };
    const selections: { asset_ids: string[]; hidden: boolean }[] = [];
    let source: any, plan: any;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, options?: RequestInit) => {
        const body = options?.body ? JSON.parse(String(options.body)) : undefined;
        if (url.startsWith("/api/v1/path?") || url === "/api/v1/nodes/1")
          return Response.json(root);
        if (url.startsWith("/api/v1/nodes/1/children"))
          return Response.json({
            directory: root,
            items: [document],
            total: 1,
            limit: 1000,
            offset: 0,
          });
        if (url.endsWith("/photos/hidden"))
          return Response.json({
            configured: true,
            change_id: "synthetic",
            expires_at: future,
            expires_in: 300,
          });
        if (url.includes("/assets/query"))
          return Response.json({ items: [photo(1), photo(2), photo(3)], total: 3 });
        if (url.endsWith("/sources")) {
          selections.push({ asset_ids: body.photos.asset_ids, hidden: body.photos.hidden });
          source = {
            id: body.operation_id,
            request_sha256: hash,
            kind: "photos",
            state: "sealed",
            member_hash: hash,
            total: body.photos.asset_ids.length,
            source_bytes: 12 * body.photos.asset_ids.length,
            created_at: "2026-01-01T00:00:00Z",
            expires_at: future,
          };
          return Response.json(source);
        }
        if (url.endsWith("/plans")) {
          plan = {
            format: "docbank-bundle-v1",
            id: body.operation_id,
            vault_id: "synthetic",
            toolchain: "go1.27",
            source,
            roles: body.roles,
            photo_render: body.photo_render,
            fingerprint: hash,
            total: source.total,
            role_entries: source.total,
            role_bytes: source.source_bytes,
            metadata_bytes: 100,
            created_at: source.created_at,
            expires_at: future,
          };
          return Response.json(plan);
        }
        if (url.endsWith("/preview"))
          return Response.json({
            plan_id: plan.id,
            fingerprint: hash,
            member_hash: hash,
            total: source.total,
            roles: [
              {
                role: "photo_rendered",
                available_members: source.total,
                unavailable_members: 0,
                files: source.total,
                bytes: source.source_bytes,
              },
            ],
          });
        if (url.endsWith("/jobs"))
          return Response.json({
            id: body.operation_id,
            plan_id: plan.id,
            fingerprint: hash,
            state: "completed",
            receipt: {
              format: "docbank-bundle-v1",
              plan_fingerprint: hash,
              sha256: hash,
              size: 512,
              entries: 4,
            },
            sequence: 1,
            completed_roles: 1,
            completed_bytes: 12,
            attempt: 1,
            created_at: source.created_at,
            deadline: future,
            expires_at: future,
          });
        if (options?.method === "DELETE") {
          return new Response(null, { status: 204 });
        }
        return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
      }),
    );
    render(App);
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Prepare" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Start reviewed export" }));
    await screen.findByRole("button", { name: "Download verified ZIP" });
    await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
    await fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 3.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
    await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
    await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    await screen.findByRole("cell", { name: "Synthetic document.txt" });
    await fireEvent.click(screen.getByRole("button", { name: "Export" }));
    expect(screen.getByText("Photo export")).toBeTruthy();
    expect(screen.getByText("1 photo")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Prepare another export" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Close export" })).toBeNull());
    await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    if (hidden) await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
    const third = await screen.findByRole("checkbox", { name: "Select photo Photo 3.jpg" });
    const clear = screen.queryByRole("button", { name: "Clear selection" });
    if (clear) await fireEvent.click(clear);
    await fireEvent.click(third);
    await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Prepare" }));
    await screen.findByRole("button", { name: "Start reviewed export" });
    expect(selections).toEqual([
      { asset_ids: ["photo-1"], hidden },
      { asset_ids: ["photo-3"], hidden },
    ]);
  },
);

async function watcherFixture({
  configured = false,
  expiry,
}: { configured?: boolean; expiry?: string } = {}) {
  const album = photoAlbum();
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  storage();
  let refresh!: Parameters<typeof photoEvents.watchPhotoEvents>[1];
  let fail!: Parameters<typeof photoEvents.watchPhotoEvents>[2];
  let connected!: NonNullable<Parameters<typeof photoEvents.watchPhotoEvents>[3]>;
  let recovered!: NonNullable<Parameters<typeof photoEvents.watchPhotoEvents>[4]>;
  const stop = vi.fn();
  const cursors = vi.fn();
  const watch = vi
    .spyOn(photoEvents, "watchPhotoEvents")
    .mockImplementation(
      (_session, liveRefresh, onfailure, onconnected, onrefreshed, pollCursor) => {
        pollCursor?.(cursors);
        recovered = onrefreshed!;
        refresh = liveRefresh;
        fail = onfailure;
        connected = onconnected!;
        return stop;
      },
    );
  const access = {
    configured,
    change_id: "first",
    change_cursor: "initial",
    expires_at: expiry,
    expires_in: 300,
  };
  const rows = [photo(1)];
  const hiddenRows = [photo(2)];
  vi.stubGlobal("fetch", async (url: string) => {
    if (url.endsWith("/photos/hidden")) return albumResponse(access);
    if (url.endsWith("/photos/albums")) return albumResponse([album]);
    if (url.includes("/nodes/1"))
      return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  });
  const stores = vi
    .spyOn(Photos.prototype, "refresh")
    .mockImplementation(async function (this: Photos) {
      this.started = true;
      this.items = this.hidden ? hiddenRows : rows;
      this.authored.revalidate(new Set(this.items.map((item) => item.asset_id)));
      return true;
    });
  vi.spyOn(Photos.prototype, "reloadCounts").mockResolvedValue(undefined);
  const workspaceRefresh = vi.fn().mockResolvedValue(undefined);
  let workspaceProps!: Parameters<typeof photoWorkspace.default>[1];
  const mounts = vi.spyOn(photoWorkspace, "default");
  mounts.mockImplementation((_anchor, inputs) => {
    workspaceProps = inputs;
    return { refresh: workspaceRefresh.bind(inputs.photos), captureReturn: vi.fn() };
  });
  render(App);
  await waitFor(() => expect(mounts).toHaveBeenCalledOnce());
  workspaceProps.photos.started = true;
  workspaceProps.photos.items = rows;
  return {
    album,
    access,
    cursors,
    rows,
    stores,
    workspaceRefresh,
    mounts,
    watch,
    stop,
    get workspaceProps() {
      return workspaceProps;
    },
    get refresh() {
      return refresh;
    },
    get fail() {
      return fail;
    },
    get connected() {
      return connected;
    },
    get recovered() {
      return recovered;
    },
  };
}

it.each(["retained", "pending"])(
  "rechecks %s metadata after a mixed visibility batch",
  async (state) => {
    const fixture = await watcherFixture();
    await act(async () => fixture.workspaceProps.onphotochange?.("photo-1", "open"));
    await waitFor(() => expect(fixture.workspaceProps.photoID).toBe("photo-1"));
    const editor = fixture.workspaceProps.photos.authored;
    const before = preparedPhoto(1);
    editor.inspectedID = before.asset_id;
    const latest = { ...before, revision: 8 };
    let finish!: () => void;
    const prepare = vi.spyOn(api, "preparePhotoAuthored");
    let pending: Promise<void> | undefined;
    if (state === "pending") {
      prepare.mockImplementationOnce(async () => {
        await new Promise<void>((resolve) => (finish = resolve));
        return { assets: [], targets: [before] };
      });
      pending = editor.prepare({ asset_ids: [before.asset_id] });
      fixture.workspaceRefresh.mockImplementationOnce(async () => {
        editor.conceal();
        finish();
        await pending;
        editor.revalidate(new Set([before.asset_id]), { asset_ids: [before.asset_id] });
        return true;
      });
    } else {
      editor.targets = [before];
    }
    prepare.mockResolvedValue({ assets: [], targets: [latest] });
    editor.stage({ caption: "Local draft" });
    fixture.access.change_id = "second";
    const batch = {
      assets: new Map([
        [before.asset_id, 8],
        ["photo-2", 2],
      ]),
      refetch: true,
    };
    const refreshing = fixture.refresh(batch);
    await act(async () => refreshing);
    expect(editor.patch.caption).toBe("Local draft");
    expect(editor.conflict).toBe(true);
    expect(prepare).toHaveBeenCalledTimes(state === "pending" ? 2 : 1);
    await editor.reload();
    expect(editor.targets[0]!.revision).toBe(8);
    expect(editor.conflict).toBe(false);
    const edit = vi
      .spyOn(api, "editPhotoAuthored")
      .mockResolvedValue({ receipt_id: "", before: [], after: [] });
    await editor.edit({}, {}, false, vi.fn());
    expect(edit.mock.calls[0]![0].targets[0]!.revision).toBe(8);
  },
);

it.each([1, 8])(
  "reconciles retained Library metadata from Albums at revision %s",
  async (revision) => {
    const fixture = await watcherFixture();
    const library = fixture.workspaceProps.photos;
    library.selectedIDs = new Set(["photo-1"]);
    library.authored.targets = [preparedPhoto(1)];
    await fireEvent.click(screen.getByRole("button", { name: "Albums" }));
    await screen.findByRole("main", { name: "Albums" });
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockResolvedValue({ assets: [], targets: [{ ...preparedPhoto(1), revision }] });
    await act(async () =>
      fixture.refresh({ assets: new Map([["photo-1", revision]]), refetch: false }),
    );
    expect(prepare).toHaveBeenCalledOnce();
    expect(library.authored.targets[0]!.revision).toBe(revision);
    expect(library.authored.conflict).toBe(false);
    await fireEvent.click(screen.getByRole("button", { name: "Library" }));
    await waitFor(() => expect(fixture.workspaceProps.photos).toBe(library));
    expect(library.selectedIDs.has("photo-1")).toBe(true);
  },
);

it("retries an album refresh after failure without another event", async () => {
  const fixture = await watcherFixture();
  const load = vi
    .spyOn(api, "listPhotoAlbums")
    .mockRejectedValueOnce(new Error("Albums unavailable"))
    .mockResolvedValue([{ ...fixture.album, name: "Updated album" }]);
  vi.stubGlobal("navigator", {});
  const failure = vi.fn();
  const stop = realWatchPhotoEvents(
    "synthetic",
    fixture.refresh,
    failure,
    undefined,
    undefined,
    (receive) => receive("changed"),
  );
  try {
    await waitFor(() => expect(failure).toHaveBeenCalledOnce());
    await waitFor(() => expect(load).toHaveBeenCalledTimes(2), { timeout: 3000 });
    await fireEvent.click(screen.getByRole("button", { name: "Albums" }));
    await screen.findByRole("button", { name: /Updated album/ });
  } finally {
    stop();
  }
});

it("forwards the Hidden view access poll cursor", async () => {
  const fixture = await watcherFixture({
    configured: true,
    expiry: new Date(Date.now() + 300000).toISOString(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await waitFor(() => expect(fixture.workspaceProps.photos.hidden).toBe(true));
  fixture.access.change_cursor = "hidden-change";
  await waitFor(() => expect(fixture.cursors).toHaveBeenCalledWith("hidden-change"), {
    timeout: 4000,
  });
});
