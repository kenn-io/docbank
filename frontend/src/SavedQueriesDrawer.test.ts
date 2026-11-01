import { createHash } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SavedQueriesDrawer from "./SavedQueriesDrawer.svelte";
import { parseQuery } from "./query.js";
import { clearToasts, getToasts } from "./toast.svelte.js";

const canonical =
  '{"filters":{"extensions":["pdf"],"no_tags":true},"mode":"hybrid","sort":{"direction":"desc","field":"size"},"syntax":"advanced","text":"alpha OR beta","v":1}';
const record = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "Review PDFs",
  description: "Synthetic query",
  kind: "query",
  payload: JSON.parse(canonical),
  revision: 2,
  fingerprint: `sha256:${createHash("sha256").update(canonical).digest("hex")}`,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};
const json = (value: unknown, revision = 2) =>
  new Response(JSON.stringify(value), { headers: { ETag: `"${revision}"` } });

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
  clearToasts();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function open(onload = vi.fn()) {
  render(SavedQueriesDrawer, {
    session: "session",
    initialQuery: parseQuery("{}"),
    onload,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  return onload;
}

it("loads a complete definition and saves it without silently executing it", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method === "POST") {
      const body = JSON.parse(String(request.body));
      expect(body.payload).toEqual(JSON.parse(canonical));
      return json({ ...record, ...body, revision: 1 }, 1);
    }
    return json({ items: [record], total: 1, limit: 100, offset: 0 });
  });
  const onload = open();
  await fireEvent.click(await screen.findByRole("button", { name: "Rename Review PDFs" }));
  await fireEvent.click(screen.getByText("Edit as JSON"));
  expect(
    JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value),
  ).toEqual(JSON.parse(canonical));
  await fireEvent.click(screen.getByRole("button", { name: "Keep query draft" }));
  expect(onload).toHaveBeenCalledWith(parseQuery(canonical));
  onload.mockClear();
  await fireEvent.input(screen.getByLabelText("Definition name"), {
    target: { value: "Copy of review" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Save as new" }));
  await screen.findByText(/Saved Copy of review/);
  expect(onload).not.toHaveBeenCalled();
  expect(fetch.mock.calls.some(([url]) => String(url).includes("/search"))).toBe(false);
});

it("preserves an unrelated unsaved editor when deleting a definition", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) =>
    request?.method === "DELETE"
      ? json(record)
      : json({ items: [record], total: 1, limit: 100, offset: 0 }),
  );
  open();
  await fireEvent.click(screen.getByRole("button", { name: "New highlight list" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), {
    target: { value: "Unfinished query" },
  });
  await fireEvent.input(screen.getByLabelText("Term 1"), { target: { value: "retain me" } });
  await fireEvent.click(await screen.findByRole("button", { name: "Delete Review PDFs" }));
  await fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));
  await screen.findByText(/^Deleted Review PDFs/);
  expect((screen.getByLabelText("Definition name") as HTMLInputElement).value).toBe(
    "Unfinished query",
  );
  expect((screen.getByLabelText("Term 1") as HTMLInputElement).value).toBe("retain me");
});

it.each(["Save as new", "Save changes"])(
  "reloads committed NFC-divergent names after %s before another save",
  async (action) => {
    let current = record;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
      if (request?.method && request.method !== "GET") {
        current = { ...record, name: "\u1ea0", revision: action === "Save as new" ? 1 : 3 };
        return json(current, current.revision);
      }
      return json({ items: [current], total: 1, limit: 100, offset: 0 });
    });
    open();
    await fireEvent.click(await screen.findByRole("button", { name: "Rename Review PDFs" }));
    await fireEvent.input(screen.getByLabelText("Definition name"), {
      target: { value: "\u{20041}\u0323" },
    });
    await fireEvent.click(screen.getByRole("button", { name: action }));
    await screen.findByRole("alert");
    await screen.findByRole("button", { name: "Rename \u1ea0" });
    expect(
      (screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(
      (screen.getByRole("button", { name: "Save as new" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    await fireEvent.click(screen.getByRole("button", { name: "Rename \u1ea0" }));
    expect((screen.getByLabelText("Definition name") as HTMLInputElement).value).toBe("\u1ea0");
    expect(
      (screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled,
    ).toBe(false);
  },
);

it("keeps stale rename and named revision-bound deletion failures visible", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method && request.method !== "GET")
      return new Response(
        JSON.stringify({
          detail: "Changed elsewhere; reload the definition",
          code: "stale_revision",
        }),
        { status: 412 },
      );
    return json({ items: [record], total: 1, limit: 100, offset: 0 });
  });
  open();
  await fireEvent.click(await screen.findByRole("button", { name: "Rename Review PDFs" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Renamed" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Changed elsewhere");
  await fireEvent.click(screen.getByRole("button", { name: "Delete Review PDFs" }));
  await fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Changed elsewhere");
  expect(screen.queryByText(/^Deleted /)).toBeNull();
  const mutations = fetch.mock.calls.filter(
    ([, request]) => request?.method && request.method !== "GET",
  );
  expect(mutations).toHaveLength(2);
  expect(new Headers(mutations[1][1]?.headers).get("If-Match")).toBe('"2"');
});

it("edits bounded highlight terms without offering execution", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ items: [], total: 0, limit: 100, offset: 0 }),
  );
  open();
  await fireEvent.click(screen.getByRole("button", { name: "New highlight list" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), {
    target: { value: "Key terms" },
  });
  await fireEvent.input(screen.getByLabelText("Term 1"), { target: { value: "alpha" } });
  await fireEvent.input(screen.getByLabelText("Color 1"), { target: { value: "red" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save as new" }));
  expect((await screen.findByRole("alert")).textContent).toContain("#rrggbb");
  expect(screen.queryByRole("button", { name: /run/i })).toBeNull();
  expect(screen.queryByRole("button", { name: "Keep query draft" })).toBeNull();
});

it("opens the exact saved payload through the explicit query-editor action", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    json({ items: [record], total: 1, limit: 100, offset: 0 }),
  );
  const onopenquery = vi.fn();
  render(SavedQueriesDrawer, {
    session: "session",
    initialQuery: parseQuery("{}"),
    onload: vi.fn(),
    onopenquery,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Run Review PDFs" }));
  expect(onopenquery).toHaveBeenCalledWith(JSON.parse(canonical));
});

it("reports a change that finishes after the Searches page is left", async () => {
  const onauthfailure = vi.fn();
  const settle: Array<(response: Response) => void> = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) =>
    request?.method && request.method !== "GET"
      ? new Promise<Response>((resolve) => settle.push(resolve))
      : json({ items: [record], total: 1, limit: 100, offset: 0 }),
  );
  for (const response of [
    new Response(JSON.stringify({ detail: "Changed elsewhere", code: "stale_revision" }), {
      status: 412,
    }),
    json({ ...record, name: "Renamed", revision: 3 }, 3),
    new Response(JSON.stringify({ detail: "Session expired" }), { status: 401 }),
  ]) {
    const view = render(SavedQueriesDrawer, {
      session: "session",
      initialQuery: parseQuery("{}"),
      onload: vi.fn(),
      onclose: vi.fn(),
      onauthfailure,
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Rename Review PDFs" }));
    await fireEvent.input(screen.getByLabelText("Definition name"), {
      target: { value: "Renamed" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await vi.waitFor(() => expect(settle).toHaveLength(1));
    view.unmount();
    settle.shift()!(response);
  }
  await vi.waitFor(() =>
    expect(getToasts().map(({ message, tone }) => ({ message, tone }))).toEqual([
      { message: expect.stringContaining("Changed elsewhere"), tone: "danger" },
      { message: "Saved Renamed.", tone: "neutral" },
    ]),
  );
  await vi.waitFor(() =>
    expect(onauthfailure).toHaveBeenCalledWith(expect.objectContaining({ status: 401 })),
  );
});

it("refreshes a reopened Searches page when an earlier change finishes", async () => {
  const renamed = { ...record, name: "Renamed", revision: 3 };
  let finish!: () => void;
  let saved = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method && request.method !== "GET")
      return new Promise<Response>((resolve) => {
        finish = () => {
          saved = true;
          resolve(json(renamed, 3));
        };
      });
    return json({ items: [saved ? renamed : record], total: 1, limit: 100, offset: 0 });
  });
  const first = render(SavedQueriesDrawer, {
    session: "session",
    initialQuery: parseQuery("{}"),
    onload: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Rename Review PDFs" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Renamed" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  first.unmount();
  open();
  expect(await screen.findByRole("button", { name: "Rename Review PDFs" })).toBeTruthy();

  finish();
  expect(await screen.findByRole("button", { name: "Rename Renamed" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Rename Review PDFs" })).toBeNull();
});
