import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PageViewer from "./PageViewer.svelte";
import VerifiedPreview from "./VerifiedPreview.svelte";
import { PageViewerSession, type PageViewerState } from "./pageViewer.js";
import { pageDigest } from "./pages.js";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
const source: SelectedSource = {
  kind: "live",
  key: "pdf-source",
  nodeID: 7,
  mutationRevision: 2,
  versionID: "00000000-0000-4000-8000-000000000001",
  blobHash: "a".repeat(64),
  size: 100,
  mimeType: "application/pdf",
  name: "mixed.pdf",
  path: "/mixed.pdf",
  modifiedAt: "2026-09-11T12:00:00Z",
};
const binding = {
  node_id: 7,
  revision: 2,
  source: { version_id: source.versionID, sha256: source.blobHash, size: source.size },
};
const empty = {
  runtime_available: true,
  inventory: { source: binding.source, page_count: 0, frames: [], images: [], recipes: [] },
};
const response = (v: unknown) => new Response(JSON.stringify(v));
function job(id: string, state = "queued") {
  const request = { ...binding, pages: [1], dpi: 144, runtime_fingerprint: "c".repeat(64) };
  const canonical = JSON.stringify(request, (_k, value) =>
    value && typeof value === "object" && !Array.isArray(value)
      ? Object.fromEntries(Object.entries(value).sort())
      : value,
  );
  return {
    id,
    state,
    request,
    request_sha256: createHash("sha256").update(canonical).digest("hex"),
    results: [],
    failure_code: "",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
}
const props = { session: "session", source, authorizationRevision: 2, onauthfailure: vi.fn() };

it("reads missing inventory without rendering until the explicit action, and owns cancellation", async () => {
  const paths: string[] = [];
  let operation = "";
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    paths.push(path);
    if (path.endsWith("inventory")) return response(empty);
    if (path.endsWith("/jobs")) {
      const body = JSON.parse(String(init?.body));
      operation = body.operation_id;
      expect(body.pages).toEqual([1]);
      expect(body.dpi).toBe(144);
      return response(job(operation));
    }
    if (path.endsWith("/cancel")) return response(job(operation, "canceled"));
    return response(job(operation));
  });
  const view = render(PageViewer, props);
  await screen.findByText(/No page inventory/);
  expect(paths).toEqual(["/api/v1/pages/inventory"]);
  await fireEvent.click(screen.getByRole("button", { name: "Render page 1" }));
  await screen.findByRole("button", { name: "Cancel render" });
  await view.rerender({ ...props, source: { ...source } });
  expect(paths.filter(path => path.endsWith("inventory"))).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Cancel render" }));
  await screen.findByText(/Render canceled/);
  expect(paths.some((p) => p.endsWith(`/${operation}/cancel`))).toBe(true);
  const canceledOperation = operation;
  await fireEvent.click(screen.getByRole("button", { name: "Refresh pages" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
  await screen.findByRole("button", { name: "Cancel render" });
  expect(operation).not.toBe(canceledOperation);
});

for (const [state, failureCode] of [["failed", "interrupted"], ["failed", "limit"], ["canceled", ""]]) {
  it(`uses a new operation to retry a ${state} render (${failureCode})`, async () => {
    const operations: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (String(input).endsWith("inventory")) return response(empty);
      const id = JSON.parse(String(init?.body)).operation_id;
      operations.push(id);
      return response({ ...job(id, state), failure_code: failureCode });
    });
    render(PageViewer, props);
    await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Refresh pages" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
    await screen.findByRole("button", { name: "Refresh pages" });
    expect(operations).toHaveLength(2);
    expect(operations[1]).not.toBe(operations[0]);
  });
}

it("preserves the operation UUID after an ambiguous response and never owns a reused external job", async () => {
  const operations: string[] = [],
    paths: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    paths.push(path);
    if (path.endsWith("inventory")) return response(empty);
    operations.push(JSON.parse(String(init?.body)).operation_id);
    if (operations.length === 1) throw new TypeError("connection lost");
    return response(job("00000000-0000-4000-8000-000000000009"));
  });
  const view = render(PageViewer, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Retry request" }));
  await screen.findByText(/Another request is rendering/);
  expect(operations).toHaveLength(2);
  expect(operations[0]).toBe(operations[1]);
  expect(screen.queryByRole("button", { name: "Cancel render" })).toBeNull();
  view.unmount();
  expect(paths.filter((p) => p.includes("/cancel"))).toHaveLength(0);
});

it("ignores late inventory after a changed source and reports runtime absence separately", async () => {
  let release!: (v: Response) => void;
  let signal: AbortSignal | null | undefined;
  vi.spyOn(globalThis, "fetch")
    .mockImplementationOnce((_input, init) => {
      signal = init?.signal;
      return new Promise((resolve) => {
        release = resolve;
      });
    })
    .mockResolvedValue(
      response({
        ...empty,
        runtime_available: false,
        inventory: { ...empty.inventory, source: { ...binding.source, size: 101 } },
      }),
    );
  const view = render(PageViewer, props);
  await waitFor(() => expect(release).toBeTruthy());
  await view.rerender({ ...props, source: { ...source, key: "new", size: 101 } });
  await screen.findByText(/Page runtime unavailable/);
  release(response(empty));
  await waitFor(() => expect(signal?.aborted).toBe(true));
  expect(screen.queryByRole("button", { name: "Render page 1" })).toBeNull();
});

function renderedInventory(dpis = [144]) {
  const digest = (v: unknown) =>
    createHash("sha256")
      .update(
        JSON.stringify(v, (_k, value) =>
          value && typeof value === "object" && !Array.isArray(value)
            ? Object.fromEntries(Object.entries(value).sort())
            : value,
        ),
      )
      .digest("hex");
  const recipes = dpis.map((dpi, index) => ({
    contract: "page-image-v1",
    dpi,
    format: "png",
    renderer_identity: { executable: "synthetic-renderer", version: String(index + 1), options: ["rgb"] },
  }));
  const frames = [
    {
      contract: "page-frame-v1",
      source: binding.source,
      page: 1,
      media_box: [0, 0, 720000, 1440000],
      crop_box: [0, 0, 720000, 1440000],
      rotation: 0,
      width: 10000,
      height: 20000,
      input_units: "point/10000",
      output_units: "inch/10000",
      axes: "top-left,x-right,y-down",
      transform: [
        { numerator: 1, denominator: 72 },
        { numerator: 0, denominator: 1 },
        { numerator: 0, denominator: 1 },
        { numerator: -1, denominator: 72 },
        { numerator: 0, denominator: 1 },
        { numerator: 20000, denominator: 1 },
      ],
    },
    {
      contract: "page-frame-v1",
      source: binding.source,
      page: 2,
      media_box: [0, 0, 1440000, 720000],
      crop_box: [0, 0, 1440000, 720000],
      rotation: 0,
      width: 20000,
      height: 10000,
      input_units: "point/10000",
      output_units: "inch/10000",
      axes: "top-left,x-right,y-down",
      transform: [
        { numerator: 1, denominator: 72 },
        { numerator: 0, denominator: 1 },
        { numerator: 0, denominator: 1 },
        { numerator: -1, denominator: 72 },
        { numerator: 0, denominator: 1 },
        { numerator: 10000, denominator: 1 },
      ],
    },
  ];
  const bytes = dpis.flatMap(dpi => [[dpi, dpi * 2], [dpi * 2, dpi]]).map(([w, h]) => {
    const b = new Uint8Array(33);
    b.set([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82]);
    const v = new DataView(b.buffer);
    v.setUint32(16, w!);
    v.setUint32(20, h!);
    return b;
  });
  const images = recipes.flatMap((recipe, recipeIndex) => frames.map((f, i) => ({
    contract: "page-image-v1",
    source: binding.source,
    page: i + 1,
    frame_sha256: digest(f),
    recipe_sha256: digest(recipe),
    sha256: createHash("sha256").update(bytes[recipeIndex * 2 + i]!).digest("hex"),
    size: 33,
    width: i === 0 ? recipe.dpi : recipe.dpi * 2,
    height: i === 0 ? recipe.dpi * 2 : recipe.dpi,
  })));
  const inventory = {
    runtime_available: false,
    inventory: {
      source: binding.source,
      page_count: 2,
      frames: frames.map((frame) => ({ frame, sha256: digest(frame) })),
      recipes: recipes.map(recipe => ({ recipe, sha256: digest(recipe) })),
      images,
    },
  };
  const imageResponse = (index: number) =>
    new Response(bytes[index]!, {
      headers: {
        "Content-Type": "image/png",
        "Content-Length": "33",
        "X-Docbank-Page-Frame": images[index]!.frame_sha256,
        "X-Docbank-Page-Recipe": images[index]!.recipe_sha256,
        "X-Docbank-Page-SHA256": images[index]!.sha256,
        "X-Docbank-Page-DPI": String(recipes[Math.floor(index / 2)]!.dpi),
        "Content-Digest": `sha-256=:${createHash("sha256").update(bytes[index]!).digest("base64")}:`,
      },
    });
  return { inventory, imageResponse };
}

it("navigates retained mixed-size pages within bounds and revokes URLs on page and tab changes", async () => {
  const fixture = renderedInventory(),
    revoke = vi.fn(),
    close = vi.fn();
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  let sequence = 0,
    decode = 0;
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => `blob:page-${++sequence}`),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: revoke });
  vi.stubGlobal(
    "createImageBitmap",
    vi.fn(async () =>
      ++decode === 1 ? { width: 144, height: 288, close } : { width: 288, height: 144, close },
    ),
  );
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith("inventory")) return response(fixture.inventory);
    if (url.includes("/pages/image"))
      return fixture.imageResponse(
        Number(new URL(url, "http://localhost").searchParams.get("page")) - 1,
      );
    return response({ items: [], truncated: false });
  });
  const view = render(VerifiedPreview, props);
  const first = await screen.findByRole("img", { name: "Verified page 1 of mixed.pdf" });
  expect(first.parentElement?.style.aspectRatio).toBe("10000 / 20000");
  expect(screen.getByRole("button", { name: "Previous page" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  expect(screen.queryByRole("img", { name: "Verified page 1 of mixed.pdf" })).toBeNull();
  const second = await screen.findByRole("img", { name: "Verified page 2 of mixed.pdf" });
  expect(second.parentElement?.style.aspectRatio).toBe("20000 / 10000");
  expect(revoke).toHaveBeenCalledWith("blob:page-1");
  expect(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(screen.getByRole("combobox", { name: /^Zoom:/ }));
  await fireEvent.click(screen.getByRole("option", { name: "150%" }));
  await view.rerender({ ...props, source: { ...source, name: "renamed.pdf" } });
  expect(await screen.findByRole("img", { name: "Verified page 2 of renamed.pdf" })).toBe(second);
  expect(second.parentElement?.style.width).toBe("288px");
  expect(revoke).not.toHaveBeenCalledWith("blob:page-2");
  await fireEvent.click(screen.getByRole("tab", { name: "Duplicates" }));
  expect(screen.queryByRole("img")).toBeNull();
  expect(revoke).toHaveBeenCalledWith("blob:page-2");
  expect(close).toHaveBeenCalledTimes(2);
  expect(fetcher.mock.calls.filter(([url]) => String(url).endsWith("/jobs"))).toHaveLength(0);
});

it("does not install an old page after late image bytes arrive", async () => {
  const fixture = renderedInventory();
  let release!: (v: Response) => void;
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:new-page"),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  vi.stubGlobal(
    "createImageBitmap",
    vi.fn(async () => ({ width: 288, height: 144, close: vi.fn() })),
  );
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith("inventory")) return response(fixture.inventory);
    if (new URL(url, "http://localhost").searchParams.get("page") === "1")
      return new Promise((resolve) => {
        release = resolve;
      });
    return fixture.imageResponse(1);
  });
  render(PageViewer, props);
  await waitFor(() => expect(release).toBeTruthy());
  await fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await screen.findByRole("img", { name: "Verified page 2 of mixed.pdf" });
  release(fixture.imageResponse(0));
  await waitFor(() =>
    expect(screen.queryByRole("img", { name: "Verified page 1 of mixed.pdf" })).toBeNull(),
  );
  expect(URL.createObjectURL).toHaveBeenCalledOnce();
});

for (const { dpis, expectedDPI, runtime, png = false } of [
  { dpis: [72, 144, 300], expectedDPI: 144, runtime: true },
  { dpis: [300, 144, 72], expectedDPI: 144, runtime: true },
  { dpis: [72, 300], expectedDPI: 300, runtime: true },
  { dpis: [72], expectedDPI: 72, runtime: true },
  { dpis: [72, 300], expectedDPI: 300, runtime: false },
  { dpis: [254, 254], expectedDPI: 254, runtime: true, png: true },
]) {
  it(`selects ${expectedDPI} DPI from ${dpis} with runtime=${runtime}, PNG=${png}`, async () => {
    const fixture = renderedInventory(dpis);
    const inventory = fixture.inventory.inventory;
    const images = inventory.images;
    fixture.inventory.runtime_available = runtime;
    if (png) {
      const frame = {
        ...inventory.frames[0]!.frame, input_units: "pixel",
        pixel_width: 254, pixel_height: 508, pixels_per_metre_x: 10000, pixels_per_metre_y: 10000,
        transform: [
          { numerator: 5000, denominator: 127 }, { numerator: 0, denominator: 1 },
          { numerator: 0, denominator: 1 }, { numerator: 5000, denominator: 127 },
          { numerator: 0, denominator: 1 }, { numerator: 0, denominator: 1 },
        ],
      };
      inventory.page_count = 1;
      inventory.frames = [{ frame, sha256: pageDigest(frame) }];
      inventory.images = images.filter(image => image.page === 1);
      for (const image of inventory.images) image.frame_sha256 = pageDigest(frame);
    }
    let selectedImage = images[0]!;
    const requests: { dpi: number; pages: number[] }[] = [];
    Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:retained") });
    Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
    vi.stubGlobal("createImageBitmap", vi.fn(async () => ({
      width: selectedImage.width, height: selectedImage.height, close: vi.fn(),
    })));
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (url.endsWith("inventory")) return response(fixture.inventory);
      if (url.endsWith("/jobs")) {
        const request = JSON.parse(String(init?.body));
        requests.push(request);
        return response(job(request.operation_id));
      }
      const query = new URL(url, "http://localhost").searchParams;
      const index = images.findIndex(image => image.recipe_sha256 === query.get("recipe_sha256") && image.page === Number(query.get("page")));
      selectedImage = images[index]!;
      return fixture.imageResponse(index);
    });
    render(VerifiedPreview, { ...props, source: png ? { ...source, mimeType: "image/png", name: "native.png" } : source });
    await screen.findByRole("img", { name: `Verified page 1 of ${png ? "native.png" : "mixed.pdf"}` });
    expect(inventory.recipes.find(recipe => recipe.sha256 === selectedImage.recipe_sha256)!.recipe.dpi).toBe(expectedDPI);
    expect(requests).toHaveLength(0);
    const renderButton = screen.queryByRole("button", { name: "Render page 1 at 144 DPI" });
    if (!png && !dpis.includes(144) && runtime) {
      expect(renderButton).toBeTruthy();
      await fireEvent.click(renderButton!);
      await screen.findByRole("button", { name: "Cancel render" });
      expect(requests).toMatchObject([{ dpi: 144, pages: [1] }]);
    } else {
      expect(renderButton).toBeNull();
    }
  });
}

it.each(["page_selection_stale", "page_limit", "page_unsupported"])(
  "offers refresh instead of resubmitting deterministic %s errors", async code => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async input => String(input).endsWith("inventory")
      ? response(empty) : new Response(JSON.stringify({ code, detail: "Rejected render" }), { status: 422 }));
    render(PageViewer, props);
    await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Retry request" })).toBeNull();
    expect(screen.getByRole("button", { name: "Refresh pages" })).toBeTruthy();
  },
);

it("bounds polling by elapsed client time and can resume its owned job after the deadline", async () => {
  vi.useFakeTimers();
  const start = new Date("2026-09-11T12:00:00Z");
  vi.setSystemTime(start);
  let state!: PageViewerState;
  let polls = 0;
  const operations: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input).endsWith("inventory")) return response(empty);
    const operation = String(input).endsWith("/jobs") ? JSON.parse(String(init?.body)).operation_id : operations[0];
    if (String(input).endsWith("/jobs")) operations.push(operation);
    else polls++;
    return response({ ...job(operation), created_at: new Date(start.getTime() - 700000).toISOString() });
  });
  const viewer = new PageViewerSession("session", source, 2, "test", next => { state = next; }, vi.fn());
  try {
    await viewer.load();
    const rendering = viewer.render();
    await vi.advanceTimersByTimeAsync(500);
    expect(state.status).toBe("rendering");
    expect(polls).toBe(1);
    await vi.advanceTimersByTimeAsync(600000);
    await rendering;
    expect(state.status).toBe("error");
    expect(state.retry).toBe(false);
    await viewer.load();
    const retrying = viewer.render();
    await vi.advanceTimersByTimeAsync(500);
    expect(state.status).toBe("rendering");
    expect(operations).toEqual([operations[0], operations[0]]);
    viewer.dispose();
    await retrying;
  } finally { viewer.dispose(); }
});

it("keeps cancellation available after losing a cancel response", async () => {
  let operation = "", cancellations = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (path.endsWith("inventory")) return response(empty);
    if (path.endsWith("/cancel")) {
      if (++cancellations === 1) throw new TypeError("Cancellation response lost");
      return response(job(operation, "canceled"));
    }
    if (path.endsWith("/jobs")) operation = JSON.parse(String(init?.body)).operation_id;
    return response(job(operation));
  });
  render(PageViewer, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Render page 1" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Cancel render" }));
  await screen.findByRole("alert");
  await fireEvent.click(screen.getByRole("button", { name: "Cancel render" }));
  await screen.findByText(/Render canceled/);
  expect(cancellations).toBe(2);
});
