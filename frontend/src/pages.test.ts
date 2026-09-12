import { createHash } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  decodePageInventory,
  decodePageJob,
  frameToDisplay,
  pageDigest,
  readPageImage,
} from "./pages.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const source = {
  version_id: "00000000-0000-4000-8000-000000000001",
  sha256: "a".repeat(64),
  size: 100,
};
const digest = (v: unknown): string =>
  createHash("sha256")
    .update(
      JSON.stringify(v, (_k, value) =>
        value && !Array.isArray(value) && typeof value === "object"
          ? Object.fromEntries(Object.entries(value).sort())
          : value,
      ),
    )
    .digest("hex");
const rat = (numerator: number, denominator = 1) => ({ numerator, denominator });
function inventory() {
  const frame = {
    contract: "page-frame-v1",
    source,
    page: 1,
    media_box: [0, 0, 6120000, 7920000],
    crop_box: [720000, 1440000, 5040000, 7200000],
    rotation: 90,
    width: 80000,
    height: 60000,
    input_units: "point/10000",
    output_units: "inch/10000",
    axes: "top-left,x-right,y-down",
    transform: [rat(0), rat(1, 72), rat(1, 72), rat(0), rat(-20000), rat(-10000)],
  };
  const recipe = {
    contract: "page-image-v1",
    dpi: 144,
    format: "png",
    renderer_identity: {
      executable: "synthetic-renderer",
      version: "1.0.0",
      options: ["rgb", "crop-visible"],
    },
  };
  return {
    runtime_available: false,
    inventory: {
      source,
      page_count: 1,
      frames: [{ frame, sha256: digest(frame) }],
      recipes: [{ recipe, sha256: digest(recipe) }],
      images: [
        {
          contract: "page-image-v1",
          source,
          page: 1,
          frame_sha256: digest(frame),
          recipe_sha256: digest(recipe),
          sha256: "b".repeat(64),
          size: 200,
          width: 1152,
          height: 864,
        },
      ],
    },
  };
}

it("matches the backend canonical numeric recipe vector", () => {
  const recipe = inventory().inventory.recipes[0]!.recipe;
  recipe.dpi = 150;
  expect(pageDigest(recipe)).toBe(
    "6b04bdb236e58425d111fcd6a5422910032d52f4fe4d90236df616648dbe161b",
  );
});

it("uses the rotated crop frame and CSS content rectangle at different zoom and DPR", () => {
  const accepted = decodePageInventory(inventory(), source);
  const frame = accepted.inventory.frames[0]!.frame;
  for (const [width, height, dpr] of [
    [400, 300, 1],
    [800, 600, 2],
    [200, 150, 3],
  ]) {
    const transform = frameToDisplay(
      frame,
      { left: 11, top: 23, width: width!, height: height! },
      dpr!,
    );
    expect(transform.toClient({ x: 40000, y: 30000 })).toEqual({
      x: 11 + width! / 2,
      y: 23 + height! / 2,
    });
    expect(transform.toFrame({ x: 11 + width! / 4, y: 23 + height! / 4 })).toEqual({
      x: 20000,
      y: 15000,
    });
  }
});

describe("rejects received authority before display", () => {
  for (const [name, change] of Object.entries({
    source: (v: ReturnType<typeof inventory>) => {
      v.inventory.source = { ...source, size: 101 };
    },
    count: (v: ReturnType<typeof inventory>) => {
      v.inventory.page_count = 2;
    },
    frame: (v: ReturnType<typeof inventory>) => {
      v.inventory.frames[0]!.sha256 = "c".repeat(64);
    },
    geometry: (v: ReturnType<typeof inventory>) => {
      v.inventory.frames[0]!.frame.width++;
      v.inventory.frames[0]!.sha256 = digest(v.inventory.frames[0]!.frame);
    },
    recipe: (v: ReturnType<typeof inventory>) => {
      v.inventory.recipes[0]!.recipe.dpi = 300;
    },
    page: (v: ReturnType<typeof inventory>) => {
      v.inventory.images[0]!.page = 2;
    },
    closure: (v: ReturnType<typeof inventory>) => {
      v.inventory.images[0]!.recipe_sha256 = "c".repeat(64);
    },
    dimensions: (v: ReturnType<typeof inventory>) => {
      v.inventory.images[0]!.width++;
    },
    size: (v: ReturnType<typeof inventory>) => {
      v.inventory.images[0]!.size = 33554433;
    },
    unsafe: (v: ReturnType<typeof inventory>) => {
      v.inventory.frames[0]!.frame.width = 9007199254740992;
    },
  }))
    it(name, () => {
      const v = inventory();
      change(v);
      expect(() => decodePageInventory(v, source)).toThrow();
    });
});

it("accepts missing inventory and partial retained images without requiring a runtime", () => {
  expect(
    decodePageInventory(
      {
        runtime_available: false,
        inventory: { source, page_count: 0, frames: [], recipes: [], images: [] },
      },
      source,
    ).inventory.page_count,
  ).toBe(0);
  const v = inventory();
  v.inventory.images = [];
  v.inventory.recipes = [];
  expect(decodePageInventory(v, source).inventory.images).toHaveLength(0);
});

it("rejects a MediaBox span outside the backend safe integer bound", () => {
  const v = inventory();
  v.inventory.frames[0]!.frame.media_box = [-9007199254740991, 0, 9007199254740991, 7920000];
  v.inventory.frames[0]!.sha256 = digest(v.inventory.frames[0]!.frame);
  v.inventory.images[0]!.frame_sha256 = v.inventory.frames[0]!.sha256;
  expect(() => decodePageInventory(v, source)).toThrow();
});

it("preserves native PNG density and rejects a substituted DPI recipe", () => {
  const frame = {
    contract: "page-frame-v1",
    source,
    page: 1,
    media_box: [0, 0, 720000, 1440000],
    crop_box: [0, 0, 720000, 1440000],
    rotation: 0,
    width: 10000,
    height: 20000,
    input_units: "pixel",
    output_units: "inch/10000",
    axes: "top-left,x-right,y-down",
    transform: [rat(5000, 127), rat(0), rat(0), rat(5000, 127), rat(0), rat(0)],
    pixel_width: 254,
    pixel_height: 508,
    pixels_per_metre_x: 10000,
    pixels_per_metre_y: 10000,
  };
  const recipe = { ...inventory().inventory.recipes[0]!.recipe, dpi: 254 };
  const receipt = {
    ...inventory().inventory.images[0]!,
    frame_sha256: digest(frame),
    recipe_sha256: digest(recipe),
    width: 254,
    height: 508,
  };
  const value = {
    runtime_available: false,
    inventory: {
      source,
      page_count: 1,
      frames: [{ frame, sha256: digest(frame) }],
      recipes: [{ recipe, sha256: digest(recipe) }],
      images: [receipt],
    },
  };
  expect(decodePageInventory(value, source).inventory.frames[0]!.frame.width).toBe(10000);
  recipe.dpi = 144;
  value.inventory.recipes[0]!.sha256 = digest(recipe);
  receipt.recipe_sha256 = digest(recipe);
  expect(() => decodePageInventory(value, source)).toThrow();
});

it("validates the exact job request and completed result count", () => {
  const request = {
    node_id: 7,
    revision: 2,
    source,
    pages: [1],
    dpi: 144,
    runtime_fingerprint: "c".repeat(64),
  };
  const job = {
    id: "00000000-0000-4000-8000-000000000002",
    request,
    request_sha256: digest(request),
    state: "queued",
    results: [],
    failure_code: "",
    created_at: "2026-09-11T12:00:00Z",
    updated_at: "2026-09-11T12:00:00Z",
  };
  expect(decodePageJob(job, request, 1, 144).state).toBe("queued");
  expect(() => decodePageJob({ ...job, state: "completed" }, request, 1, 144)).toThrow();
  expect(() => decodePageJob(job, { ...request, revision: 3 }, 1, 144)).toThrow();
  expect(() => decodePageJob(job, request, 2, 144)).toThrow();
  expect(() => decodePageJob(job, request, 1, 300)).toThrow();
});

function pngResponse() {
  const bytes = new Uint8Array(33);
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82]);
  const data = new DataView(bytes.buffer);
  data.setUint32(16, 1152);
  data.setUint32(20, 864);
  const receipt = {
    ...inventory().inventory.images[0]!,
    sha256: createHash("sha256").update(bytes).digest("hex"),
    size: bytes.length,
  };
  const headers = {
    "Content-Type": "image/png",
    "Content-Length": String(bytes.length),
    "X-Docbank-Page-Frame": receipt.frame_sha256,
    "X-Docbank-Page-Recipe": receipt.recipe_sha256,
    "X-Docbank-Page-SHA256": receipt.sha256,
    "X-Docbank-Page-DPI": "144",
    "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
  };
  return { bytes, receipt, headers };
}

it("bounds the body and verifies headers, encoded dimensions and decoded dimensions before a URL", async () => {
  const fixture = pngResponse();
  const create = vi.fn(() => "blob:page");
  vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: create }));
  const close = vi.fn();
  vi.stubGlobal(
    "createImageBitmap",
    vi.fn(async () => ({ width: 1152, height: 864, close })),
  );
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async () => new Response(fixture.bytes, { headers: fixture.headers }));
  const binding = { node_id: 7, revision: 2, source };
  const recipe = inventory().inventory.recipes[0]!.recipe;
  expect(
    await readPageImage("session", binding, fixture.receipt, recipe, new AbortController().signal),
  ).toBe("blob:page");
  expect(close).toHaveBeenCalledOnce();
  expect(create).toHaveBeenCalledOnce();
  fixture.headers["Content-Length"] = "33554433";
  await expect(
    readPageImage("session", binding, fixture.receipt, recipe, new AbortController().signal),
  ).rejects.toThrow();
  expect(create).toHaveBeenCalledOnce();
  fixture.headers["Content-Length"] = String(fixture.bytes.length);
  fixture.bytes[20] = 1;
  await expect(
    readPageImage("session", binding, fixture.receipt, recipe, new AbortController().signal),
  ).rejects.toThrow();
  expect(fetcher).toHaveBeenCalledTimes(3);
});

it("fences late image decoding and releases its decoded resource", async () => {
  const fixture = pngResponse(),
    controller = new AbortController();
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(fixture.bytes, { headers: fixture.headers }),
  );
  let release!: (v: unknown) => void;
  const close = vi.fn(),
    create = vi.fn();
  vi.stubGlobal(
    "createImageBitmap",
    vi.fn(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    ),
  );
  vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: create }));
  const pending = readPageImage(
    "session",
    { node_id: 7, revision: 2, source },
    fixture.receipt,
    inventory().inventory.recipes[0]!.recipe,
    controller.signal,
  );
  await vi.waitFor(() => expect(release).toBeTruthy());
  controller.abort();
  release({ width: 1152, height: 864, close });
  await expect(pending).rejects.toThrow();
  expect(close).toHaveBeenCalledOnce();
  expect(create).not.toHaveBeenCalled();
});

it("fences a delayed digest before allocating decoded image resources", async () => {
  const fixture = pngResponse(),
    controller = new AbortController();
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(fixture.bytes, { headers: fixture.headers }),
  );
  let release!: (v: ArrayBuffer) => void;
  vi.spyOn(crypto.subtle, "digest").mockImplementation(
    () =>
      new Promise((resolve) => {
        release = resolve;
      }),
  );
  const decode = vi.fn();
  vi.stubGlobal("createImageBitmap", decode);
  const pending = readPageImage(
    "session",
    { node_id: 7, revision: 2, source },
    fixture.receipt,
    inventory().inventory.recipes[0]!.recipe,
    controller.signal,
  );
  await vi.waitFor(() => expect(release).toBeTruthy());
  controller.abort();
  release(new ArrayBuffer(32));
  await expect(pending).rejects.toThrow();
  expect(decode).not.toHaveBeenCalled();
});

it("rejects a decoder dimension mismatch and closes the failed resource", async () => {
  const fixture = pngResponse(),
    close = vi.fn();
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(fixture.bytes, { headers: fixture.headers }),
  );
  vi.stubGlobal(
    "createImageBitmap",
    vi.fn(async () => ({ width: 999, height: 864, close })),
  );
  await expect(
    readPageImage(
      "session",
      { node_id: 7, revision: 2, source },
      fixture.receipt,
      inventory().inventory.recipes[0]!.recipe,
      new AbortController().signal,
    ),
  ).rejects.toThrow(/decoded/);
  expect(close).toHaveBeenCalledOnce();
});

for (const header of [
  "X-Docbank-Page-Frame",
  "X-Docbank-Page-Recipe",
  "X-Docbank-Page-SHA256",
  "X-Docbank-Page-DPI",
  "Content-Type",
  "Content-Digest",
]) {
  it(`rejects mismatched ${header}`, async () => {
    const fixture = pngResponse();
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(fixture.bytes, { headers: { ...fixture.headers, [header]: "wrong" } }),
    );
    const decode = vi.fn();
    vi.stubGlobal("createImageBitmap", decode);
    await expect(
      readPageImage(
        "session",
        { node_id: 7, revision: 2, source },
        fixture.receipt,
        inventory().inventory.recipes[0]!.recipe,
        new AbortController().signal,
      ),
    ).rejects.toThrow();
    expect(decode).not.toHaveBeenCalled();
  });
}

for (const delta of [-1, 1])
  it(`rejects a body with ${delta > 0 ? "extra" : "missing"} bytes`, async () => {
    const fixture = pngResponse(),
      bytes = new Uint8Array(fixture.bytes.length + delta);
    bytes.set(fixture.bytes.subarray(0, Math.min(bytes.length, fixture.bytes.length)));
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(bytes, { headers: fixture.headers }),
    );
    await expect(
      readPageImage(
        "session",
        { node_id: 7, revision: 2, source },
        fixture.receipt,
        inventory().inventory.recipes[0]!.recipe,
        new AbortController().signal,
      ),
    ).rejects.toThrow();
  });
