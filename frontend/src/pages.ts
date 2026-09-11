import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { requestJSON, requestResponse } from "./api.js";
import type { SelectedSource } from "./selectedSource.js";

export interface PageSource {
  version_id: string;
  sha256: string;
  size: number;
}
export interface PageBinding {
  node_id: number;
  revision: number;
  source: PageSource;
}
export interface PageRational {
  numerator: number;
  denominator: number;
}
export interface PageFrame {
  contract: string;
  source: PageSource;
  page: number;
  media_box: number[];
  crop_box: number[];
  rotation: number;
  width: number;
  height: number;
  input_units: string;
  output_units: string;
  axes: string;
  transform: PageRational[];
  pixel_width?: number;
  pixel_height?: number;
  pixels_per_metre_x?: number;
  pixels_per_metre_y?: number;
}
export interface PageRecipe {
  contract: string;
  dpi: number;
  format: string;
  renderer_identity: {
    executable: string;
    version: string;
    options: string[];
    runtime?: Record<string, string | number>;
  };
}
export interface PageImage {
  contract: string;
  source: PageSource;
  page: number;
  frame_sha256: string;
  recipe_sha256: string;
  sha256: string;
  size: number;
  width: number;
  height: number;
}
export interface PageInventory {
  runtime_available: boolean;
  inventory: {
    source: PageSource;
    page_count: number;
    frames: { frame: PageFrame; sha256: string }[];
    recipes: { recipe: PageRecipe; sha256: string }[];
    images: PageImage[];
  };
}
export interface PageJob {
  id: string;
  request_sha256: string;
  request: PageBinding & { pages: number[]; dpi: number; runtime_fingerprint: string };
  state: "queued" | "running" | "completed" | "canceled" | "failed";
  results: PageImage[];
  failure_code: string;
  created_at: string;
  updated_at: string;
}
const maxSource = 64 * 1024 * 1024,
  maxImage = 32 * 1024 * 1024;
const hashPattern = /^[0-9a-f]{64}$/,
  uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
function requireValue(condition: unknown, message = "Invalid page authority"): asserts condition {
  if (!condition) throw new Error(message);
}
function object(v: unknown, keys: string[], optional: string[] = []): Record<string, unknown> {
  requireValue(v !== null && typeof v === "object" && !Array.isArray(v));
  const result = v as Record<string, unknown>;
  requireValue(
    keys.every((k) => Object.hasOwn(result, k)) &&
      Object.keys(result).every((k) => keys.includes(k) || optional.includes(k)),
  );
  return result;
}
function integer(v: unknown, min = 0, max = Number.MAX_SAFE_INTEGER): number {
  requireValue(typeof v === "number" && Number.isSafeInteger(v) && v >= min && v <= max);
  return v;
}
function text(v: unknown, max = 512): string {
  requireValue(
    typeof v === "string" &&
      v.length > 0 &&
      v.trim() === v &&
      new TextEncoder().encode(v).length <= max &&
      !/[\p{Cc}\p{Cs}]/u.test(v),
  );
  return v;
}
function hash(v: unknown): string {
  requireValue(typeof v === "string" && hashPattern.test(v));
  return v;
}
function array(v: unknown, max: number): unknown[] {
  requireValue(Array.isArray(v) && v.length <= max);
  return v;
}
function same(a: unknown, b: unknown): boolean {
  return canonical(a) === canonical(b);
}
// RFC 8785 number/string serialization agrees with the backend canonical encoder.
function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v !== null && typeof v === "object")
    return `{${Object.entries(v)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([k, value]) => `${JSON.stringify(k)}:${canonical(value)}`)
      .join(",")}}`;
  return JSON.stringify(v);
}
export function pageDigest(v: unknown): string {
  return bytesToHex(sha256(new TextEncoder().encode(canonical(v))));
}
function decodeSource(v: unknown, expected?: PageSource): PageSource {
  const o = object(v, ["version_id", "sha256", "size"]);
  requireValue(typeof o.version_id === "string" && uuidPattern.test(o.version_id));
  hash(o.sha256);
  integer(o.size, 1, maxSource);
  const source = o as unknown as PageSource;
  requireValue(
    !expected || same(source, expected),
    "Page source does not match the selected version",
  );
  return source;
}
export function pageBinding(source: SelectedSource, revision: number): PageBinding {
  return {
    node_id: integer(source.nodeID, 1),
    revision: integer(revision, 1),
    source: decodeSource({
      version_id: source.versionID,
      sha256: source.blobHash,
      size: source.size,
    }),
  };
}
function round(n: bigint, d: bigint): number {
  requireValue(n > BigInt(0) && d > BigInt(0));
  return integer(Number((n * BigInt(2) + d) / (BigInt(2) * d)), 1);
}
function rational(n: bigint, d: bigint): PageRational {
  let a = n < BigInt(0) ? -n : n,
    b = d;
  while (b) [a, b] = [b, a % b];
  return {
    numerator: integer(Number(n / a), -Number.MAX_SAFE_INTEGER),
    denominator: integer(Number(d / a), 1),
  };
}
function decodeFrame(v: unknown, source: PageSource, page: number): PageFrame {
  const o = object(
    v,
    [
      "contract",
      "source",
      "page",
      "media_box",
      "crop_box",
      "rotation",
      "width",
      "height",
      "input_units",
      "output_units",
      "axes",
      "transform",
    ],
    ["pixel_width", "pixel_height", "pixels_per_metre_x", "pixels_per_metre_y"],
  );
  decodeSource(o.source, source);
  requireValue(
    o.contract === "page-frame-v1" &&
      o.page === page &&
      o.output_units === "inch/10000" &&
      o.axes === "top-left,x-right,y-down",
  );
  integer(o.width, 1);
  integer(o.height, 1);
  requireValue([0, 90, 180, 270].includes(o.rotation as number));
  const boxes = [o.media_box, o.crop_box].map((v) => {
    const a = array(v, 4);
    requireValue(a.length === 4);
    return a.map((n) => BigInt(integer(n, -Number.MAX_SAFE_INTEGER)));
  });
  const [m, c] = boxes as [bigint[], bigint[]];
  requireValue(
    boxes.every(
      (box) =>
        box[2]! - box[0]! <= BigInt(Number.MAX_SAFE_INTEGER) &&
        box[3]! - box[1]! <= BigInt(Number.MAX_SAFE_INTEGER),
    ),
  );
  requireValue(
    m[2]! > m[0]! &&
      m[3]! > m[1]! &&
      c[2]! > c[0]! &&
      c[3]! > c[1]! &&
      c[0]! >= m[0]! &&
      c[1]! >= m[1]! &&
      c[2]! <= m[2]! &&
      c[3]! <= m[3]!,
  );
  const t = array(o.transform, 6);
  requireValue(t.length === 6);
  for (const entry of t) {
    const r = object(entry, ["numerator", "denominator"]);
    integer(r.numerator, -Number.MAX_SAFE_INTEGER);
    integer(r.denominator, 1);
  }
  let expected: PageRational[], width: number, height: number;
  if (o.input_units === "pixel") {
    requireValue(page === 1 && o.rotation === 0);
    const pw = BigInt(integer(o.pixel_width, 1, 16384)),
      ph = BigInt(integer(o.pixel_height, 1, 16384));
    requireValue(pw * ph <= BigInt(40000000));
    const ppm = BigInt(integer(o.pixels_per_metre_x, 1, 4294967295));
    requireValue(o.pixels_per_metre_x === o.pixels_per_metre_y);
    const d = ppm * BigInt(127);
    width = round(pw * BigInt(50000000), d);
    height = round(ph * BigInt(50000000), d);
    requireValue(
      same(o.media_box, [
        0,
        0,
        round(pw * BigInt(3600000000), d),
        round(ph * BigInt(3600000000), d),
      ]) && same(o.crop_box, o.media_box),
    );
    expected = [
      rational(BigInt(50000000), d),
      rational(BigInt(0), BigInt(1)),
      rational(BigInt(0), BigInt(1)),
      rational(BigInt(50000000), d),
      rational(BigInt(0), BigInt(1)),
      rational(BigInt(0), BigInt(1)),
    ];
  } else {
    requireValue(
      o.input_units === "point/10000" &&
        [o.pixel_width, o.pixel_height, o.pixels_per_metre_x, o.pixels_per_metre_y].every(
          (v) => v === undefined,
        ),
    );
    let w = c[2]! - c[0]!,
      h = c[3]! - c[1]!;
    requireValue(w <= BigInt(Number.MAX_SAFE_INTEGER) && h <= BigInt(Number.MAX_SAFE_INTEGER));
    if (o.rotation === 90 || o.rotation === 270) [w, h] = [h, w];
    width = round(w, BigInt(72));
    height = round(h, BigInt(72));
    const n =
      o.rotation === 0
        ? [BigInt(1), BigInt(0), BigInt(0), -BigInt(1), -c[0]!, c[3]!]
        : o.rotation === 90
          ? [BigInt(0), BigInt(1), BigInt(1), BigInt(0), -c[1]!, -c[0]!]
          : o.rotation === 180
            ? [-BigInt(1), BigInt(0), BigInt(0), BigInt(1), c[2]!, -c[1]!]
            : [BigInt(0), -BigInt(1), -BigInt(1), BigInt(0), c[3]!, c[2]!];
    expected = n.map((v) => rational(v, BigInt(72)));
  }
  requireValue(
    o.width === width && o.height === height && same(o.transform, expected),
    "Page frame disagrees with its physical geometry",
  );
  return o as unknown as PageFrame;
}
function decodeRecipe(v: unknown): PageRecipe {
  const o = object(v, ["contract", "dpi", "format", "renderer_identity"]);
  requireValue(
    o.contract === "page-image-v1" &&
      o.format === "png" &&
      typeof o.dpi === "number" &&
      Number.isFinite(o.dpi) &&
      o.dpi > 0 &&
      o.dpi <= 1000000,
  );
  const r = object(o.renderer_identity, ["executable", "version", "options"], ["runtime"]);
  text(r.executable, 128);
  text(r.version);
  const options = array(r.options, 64);
  requireValue(options.length > 0);
  options.forEach((v) => text(v));
  if (r.runtime !== undefined) {
    const fixed = {
      platform: "linux/amd64",
      memory_enforcement: "rlimit-as",
      inspector_memory_bytes: 2147483648,
      renderer_memory_bytes: 536870912,
      max_source_bytes: maxSource,
      max_pages: 1000,
      max_output_bytes: maxImage,
      max_pixels: 40000000,
      max_axis: 16384,
      max_geometry_bytes: 16777216,
      max_diagnostic_bytes: 65536,
      phase_seconds: 60,
    };
    const rt = object(r.runtime, [
      "inspector_sha256",
      "inspector_version",
      "renderer_sha256",
      "limiter_sha256",
      "limiter_version",
      "deployment_identity",
      ...Object.keys(fixed),
    ]);
    for (const key of ["inspector_sha256", "renderer_sha256", "limiter_sha256"]) hash(rt[key]);
    for (const key of ["inspector_version", "limiter_version", "deployment_identity"])
      text(rt[key]);
    requireValue(Object.entries(fixed).every(([k, value]) => rt[k] === value));
  }
  return o as unknown as PageRecipe;
}
function decodeImage(v: unknown, source: PageSource): PageImage {
  const o = object(v, [
    "contract",
    "source",
    "page",
    "frame_sha256",
    "recipe_sha256",
    "sha256",
    "size",
    "width",
    "height",
  ]);
  requireValue(o.contract === "page-image-v1");
  decodeSource(o.source, source);
  integer(o.page, 1, 1000);
  hash(o.frame_sha256);
  hash(o.recipe_sha256);
  hash(o.sha256);
  integer(o.size, 1, maxImage);
  integer(o.width, 1, 16384);
  integer(o.height, 1, 16384);
  requireValue((o.width as number) * (o.height as number) <= 40000000);
  return o as unknown as PageImage;
}
function dimensions(frame: PageFrame, recipe: PageRecipe): [number, number] {
  if (frame.input_units === "pixel") {
    requireValue(recipe.dpi === (frame.pixels_per_metre_x! * 127) / 5000);
    return [frame.pixel_width!, frame.pixel_height!];
  }
  // Exact decimal DPI, including scientific notation, matching Go's rational conversion.
  const [mantissa, exponent = "0"] = String(recipe.dpi).split("e");
  const [whole, fraction = ""] = mantissa!.split(".");
  let n = BigInt(whole! + fraction),
    d = BigInt(10) ** BigInt(fraction.length);
  const power = Number(exponent);
  if (power >= 0) n *= BigInt(10) ** BigInt(power);
  else d *= BigInt(10) ** BigInt(-power);
  let w = BigInt(frame.crop_box[2]!) - BigInt(frame.crop_box[0]!),
    h = BigInt(frame.crop_box[3]!) - BigInt(frame.crop_box[1]!);
  if (frame.rotation === 90 || frame.rotation === 270) [w, h] = [h, w];
  const ceil = (points: bigint) =>
    Number((points * n + BigInt(720000) * d - BigInt(1)) / (BigInt(720000) * d));
  return [ceil(w), ceil(h)];
}
export function decodePageInventory(v: unknown, source: PageSource): PageInventory {
  const root = object(v, ["runtime_available", "inventory"], ["$schema"]);
  requireValue(typeof root.runtime_available === "boolean");
  const o = object(root.inventory, ["source", "page_count", "frames", "recipes", "images"]);
  decodeSource(o.source, source);
  const count = integer(o.page_count, 0, 1000);
  const frames = array(o.frames, 1000).map((value, index) => {
    const view = object(value, ["frame", "sha256"]);
    const frame = decodeFrame(view.frame, source, index + 1);
    requireValue(pageDigest(frame) === hash(view.sha256), "Invalid page frame digest");
    return { frame, sha256: view.sha256 as string };
  });
  requireValue(
    frames.length === count &&
      frames.every(
        (v) =>
          v.frame.input_units === frames[0]!.frame.input_units &&
          (v.frame.input_units !== "pixel" || count === 1),
      ),
    "Incomplete page frame inventory",
  );
  const recipes = array(o.recipes, 16000).map((value) => {
    const view = object(value, ["recipe", "sha256"]);
    const recipe = decodeRecipe(view.recipe);
    requireValue(pageDigest(recipe) === hash(view.sha256), "Invalid page recipe digest");
    return { recipe, sha256: view.sha256 as string };
  });
  requireValue(new Set(recipes.map((v) => v.sha256)).size === recipes.length);
  const seen = new Set<string>();
  const images = array(o.images, 16000).map((value) => {
    const image = decodeImage(value, source);
    const frame = frames[image.page - 1],
      recipe = recipes.find((v) => v.sha256 === image.recipe_sha256);
    requireValue(
      frame && recipe && frame.sha256 === image.frame_sha256,
      "Page image frame/recipe closure is invalid",
    );
    const size = dimensions(frame.frame, recipe.recipe);
    requireValue(
      image.width === size[0] && image.height === size[1],
      "Page image dimensions disagree with the physical frame",
    );
    const key = `${image.page}:${image.recipe_sha256}`;
    requireValue(!seen.has(key));
    seen.add(key);
    return image;
  });
  requireValue(recipes.every((r) => images.some((i) => i.recipe_sha256 === r.sha256)));
  return {
    runtime_available: root.runtime_available,
    inventory: { source, page_count: count, frames, recipes, images },
  };
}

export interface ContentRectangle {
  left: number;
  top: number;
  width: number;
  height: number;
}
export interface FramePoint {
  x: number;
  y: number;
}
// Call with the image content element's getBoundingClientRect, excluding borders,
// padding and letterboxing. Coordinates are CSS pixels; DPR never changes authority.
export function frameToDisplay(frame: PageFrame, rect: ContentRectangle, dpr = 1) {
  requireValue(
    [rect.left, rect.top, rect.width, rect.height, dpr].every(Number.isFinite) &&
      rect.width > 0 &&
      rect.height > 0 &&
      dpr > 0,
  );
  return {
    toClient: (p: FramePoint): FramePoint => ({
      x: rect.left + (p.x * rect.width) / frame.width,
      y: rect.top + (p.y * rect.height) / frame.height,
    }),
    toFrame: (p: FramePoint): FramePoint => ({
      x: ((p.x - rect.left) * frame.width) / rect.width,
      y: ((p.y - rect.top) * frame.height) / rect.height,
    }),
  };
}

export async function readPageInventory(
  session: string,
  binding: PageBinding,
  signal: AbortSignal,
): Promise<PageInventory> {
  return decodePageInventory(
    await requestJSON("/api/v1/pages/inventory", session, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ selection: binding }),
      signal,
    }),
    binding.source,
  );
}

export function decodePageJob(
  v: unknown,
  binding: PageBinding,
  page: number,
  dpi: number,
): PageJob {
  const o = object(
    v,
    [
      "id",
      "request_sha256",
      "request",
      "state",
      "results",
      "failure_code",
      "created_at",
      "updated_at",
    ],
    ["$schema"],
  );
  requireValue(typeof o.id === "string" && uuidPattern.test(o.id));
  const r = object(o.request, [
    "node_id",
    "revision",
    "source",
    "pages",
    "dpi",
    "runtime_fingerprint",
  ]);
  requireValue(
    r.node_id === binding.node_id &&
      r.revision === binding.revision &&
      same(r.pages, [integer(page, 1, 1000)]) &&
      r.dpi === dpi,
  );
  decodeSource(r.source, binding.source);
  hash(r.runtime_fingerprint);
  requireValue(pageDigest(r) === hash(o.request_sha256), "Invalid page job request digest");
  requireValue(
    ["queued", "running", "completed", "canceled", "failed"].includes(o.state as string),
  );
  requireValue(
    [
      "",
      "unavailable",
      "unsupported",
      "invalid_output",
      "stale_source",
      "interrupted",
      "storage",
    ].includes(o.failure_code as string),
  );
  requireValue((o.state === "failed") === (o.failure_code !== ""));
  for (const k of ["created_at", "updated_at"]) {
    text(o[k]);
    requireValue(Number.isFinite(Date.parse(o[k] as string)));
  }
  const results = array(o.results, 1).map((value) => decodeImage(value, binding.source));
  requireValue(
    results.every((r) => r.page === page) && (o.state !== "completed" || results.length === 1),
  );
  return { ...o, results } as unknown as PageJob;
}
export async function requestPageJob(
  session: string,
  binding: PageBinding,
  page: number,
  dpi: number,
  operation: string,
  signal: AbortSignal,
): Promise<PageJob> {
  requireValue(uuidPattern.test(operation));
  integer(page, 1, 1000);
  return decodePageJob(
    await requestJSON("/api/v1/pages/jobs", session, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ operation_id: operation, selection: binding, pages: [page], dpi }),
      signal,
    }),
    binding,
    page,
    dpi,
  );
}
export async function readPageJob(
  session: string,
  binding: PageBinding,
  job: PageJob,
  signal: AbortSignal,
  cancel = false,
): Promise<PageJob> {
  requireValue(uuidPattern.test(job.id));
  const result = decodePageJob(
    await requestJSON(`/api/v1/pages/jobs/${job.id}${cancel ? "/cancel" : ""}`, session, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ selection: binding }),
      signal,
    }),
    binding,
    job.request.pages[0]!,
    job.request.dpi,
  );
  requireValue(
    result.id === job.id && result.request_sha256 === job.request_sha256,
    "The render job changed identity",
  );
  return result;
}
export function verifyJobInventory(job: PageJob, inventory: PageInventory): void {
  for (const result of job.results) {
    requireValue(
      inventory.inventory.images.some((image) => same(image, result)),
      "The render result is missing from retained inventory",
    );
    const recipe = inventory.inventory.recipes.find(
      (r) => r.sha256 === result.recipe_sha256,
    )!.recipe;
    requireValue(job.request.dpi === 0 || job.request.dpi === recipe.dpi);
    requireValue(
      !recipe.renderer_identity.runtime ||
        pageDigest(recipe.renderer_identity.runtime) === job.request.runtime_fingerprint,
    );
  }
}

export async function readPageImage(
  session: string,
  binding: PageBinding,
  image: PageImage,
  recipe: PageRecipe,
  signal: AbortSignal,
): Promise<string> {
  decodeImage(image, binding.source);
  decodeRecipe(recipe);
  requireValue(pageDigest(recipe) === image.recipe_sha256);
  signal.throwIfAborted();
  const query = new URLSearchParams({
    node_id: String(binding.node_id),
    revision: String(binding.revision),
    version_id: binding.source.version_id,
    source_sha256: binding.source.sha256,
    source_size: String(binding.source.size),
    page: String(image.page),
    recipe_sha256: image.recipe_sha256,
    frame_sha256: image.frame_sha256,
    image_sha256: image.sha256,
  });
  const response = await requestResponse(`/api/v1/pages/image?${query}`, session, {
    signal,
    headers: { Accept: "image/png" },
  });
  const headers = response.headers;
  try {
    signal.throwIfAborted();
    requireValue(
      headers.get("Content-Type") === "image/png" &&
        headers.get("Content-Length") === String(image.size) &&
        headers.get("X-Docbank-Page-Frame") === image.frame_sha256 &&
        headers.get("X-Docbank-Page-Recipe") === image.recipe_sha256 &&
        headers.get("X-Docbank-Page-SHA256") === image.sha256 &&
        Number(headers.get("X-Docbank-Page-DPI")) === recipe.dpi,
      "Page image headers disagree with the selected receipt",
    );
  } catch (cause) {
    await response.body?.cancel();
    throw cause;
  }
  requireValue(response.body, "Page image body is missing");
  // Receipt and Content-Length are bounded before allocation. Never call blob()
  // or arrayBuffer() on the unbounded response body.
  const bytes = new Uint8Array(image.size),
    reader = response.body.getReader();
  let received = 0;
  const abort = () => {
    void reader.cancel().catch(() => undefined);
  };
  signal.addEventListener("abort", abort, { once: true });
  try {
    while (true) {
      signal.throwIfAborted();
      const next = await reader.read();
      signal.throwIfAborted();
      if (next.done) break;
      requireValue(
        received + next.value.length <= image.size,
        "Page image exceeds its received size",
      );
      bytes.set(next.value, received);
      received += next.value.length;
    }
    requireValue(received === image.size, "Page image is truncated");
  } catch (cause) {
    await reader.cancel().catch(() => undefined);
    throw cause;
  } finally {
    signal.removeEventListener("abort", abort);
    reader.releaseLock();
  }
  const actual = crypto.subtle
    ? bytesToHex(new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)))
    : bytesToHex(sha256(bytes));
  signal.throwIfAborted();
  const encodedHash = btoa(
    String.fromCharCode(...image.sha256.match(/../g)!.map((v) => parseInt(v, 16))),
  );
  requireValue(
    actual === image.sha256 && headers.get("Content-Digest") === `sha-256=:${encodedHash}:`,
    "Invalid page image digest",
  );
  const view = new DataView(bytes.buffer);
  requireValue(
    bytes.length >= 33 &&
      [137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82].every(
        (v, i) => bytes[i] === v,
      ) &&
      view.getUint32(16) === image.width &&
      view.getUint32(20) === image.height,
    "Invalid encoded page image dimensions",
  );
  const blob = new Blob([bytes], { type: "image/png" });
  let decoded: ImageBitmap;
  try {
    decoded = await createImageBitmap(blob);
  } catch {
    signal.throwIfAborted();
    throw new Error("Invalid page image: decoding failed");
  }
  try {
    signal.throwIfAborted();
    requireValue(
      decoded.width === image.width && decoded.height === image.height,
      "Invalid decoded page image dimensions",
    );
  } finally {
    decoded.close();
  }
  signal.throwIfAborted();
  return URL.createObjectURL(blob);
}
