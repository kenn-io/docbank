import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import App from "./App.svelte";
import type { ContentVersion, Node } from "./generated/docbank.js";

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
  ...root,
  id: 42,
  name: "example.txt",
  kind: "file",
  path: "/reports/example.txt",
  current_version_id: currentID,
  blob_hash: "b".repeat(64),
  size: 200,
  revision: 1001,
};
const oldVersion: ContentVersion = {
  id: oldID,
  node_id: 42,
  blob_hash: "a".repeat(64),
  size: 7,
  node_revision: 1,
  recorded_at: "2026-01-01T00:00:00Z",
  mime_type: "text/plain",
  introduced_operation_id: "33333333-3333-4333-8333-333333333333",
  transition_kind: "content_create",
};
const currentVersion: ContentVersion = {
  ...oldVersion,
  id: currentID,
  blob_hash: "b".repeat(64),
  size: 200,
  node_revision: 1001,
  transition_kind: "content_replace",
};
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

it("reports an invalid selection without requesting a version", async () => {
  history.replaceState(null, "", `/#${credentials}node=42&version=invalid`);
  const fetchMock = api();
  render(App);
  expect(await screen.findByText(/Invalid node\/version link/)).toBeTruthy();
  expect(
    fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/versions/")),
  ).toBe(false);
});

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

it("reopens exact versions on browser back and forward", async () => {
  history.replaceState(null, "", `/#${credentials}${link().slice(1)}`);
  api();
  render(App);
  await screen.findByText(oldID);
  await act(() => {
    history.pushState(null, "", `/${link(currentID)}`);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await screen.findByText(currentID);
  history.back();
  await screen.findByText(oldID);
  history.forward();
  await screen.findByText(currentID);
  expect(location.hash).toBe(link(currentID));
});
