import { afterEach, expect, it, vi } from "vitest";
import { uploadPackageZIP } from "./loadfile.js";

const container = {
  container_id: "one-shot-zip",
  format: "zip",
  state: "uploading",
  sha256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
  size: 3,
};

afterEach(() => vi.unstubAllGlobals());

it.each(["failed", "canceled"])("abandons a %s one-shot upload with a fresh cleanup signal", async (outcome) => {
  const controller = new AbortController();
  const failure = new Error("synthetic upload stopped");
  const requests: { url: string; method: string; signal?: AbortSignal | null }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options: RequestInit) => {
    expect(new Headers(options.headers).get("X-Docbank-Web-Session")).toBe("synthetic-session");
    requests.push({ url, method: options.method!, signal: options.signal });
    if (options.method === "DELETE") {
      expect(options.signal?.aborted).toBe(false);
      expect(options.signal).not.toBe(controller.signal);
      return new Response(null, { status: 204 });
    }
    return Response.json(container, { status: 201 });
  }));
  const channel = { uploadPackageContainer: async () => {
    if (outcome === "canceled") controller.abort();
    throw failure;
  } };

  await expect(uploadPackageZIP("synthetic-session", channel, new File(["abc"], "package.zip"),
    container.container_id, controller.signal, () => {})).rejects.toBe(failure);

  expect(requests.map(({ url, method }) => `${method} ${url}`)).toEqual([
    "POST /api/v1/packages/containers",
    "DELETE /api/v1/packages/containers/one-shot-zip",
  ]);
});

it("preserves the upload error when cleanup of sealed authority is refused", async () => {
  const failure = new Error("synthetic seal response lost");
  const requests: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options: RequestInit) => {
    requests.push(`${options.method} ${url}`);
    if (url.endsWith("/seal")) throw failure;
    if (options.method === "DELETE") return Response.json({ code: "mailbox_conflict" }, { status: 409 });
    return Response.json(container, { status: 201 });
  }));

  await expect(uploadPackageZIP("synthetic-session", { uploadPackageContainer: async () => {} },
    new File(["abc"], "package.zip"), container.container_id, new AbortController().signal,
    () => {})).rejects.toBe(failure);

  expect(requests).toEqual([
    "POST /api/v1/packages/containers",
    "POST /api/v1/packages/containers/one-shot-zip/seal",
    "DELETE /api/v1/packages/containers/one-shot-zip",
  ]);
});

it("returns a sealed upload without abandoning it", async () => {
  const methods: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, options: RequestInit) => {
    methods.push(options.method!);
    return Response.json({ ...container, state: url.endsWith("/seal") ? "sealed" : "uploading" });
  }));

  const result = await uploadPackageZIP("synthetic-session", { uploadPackageContainer: async () => {} },
    new File(["abc"], "package.zip"), container.container_id, new AbortController().signal, () => {});

  expect(result.state).toBe("sealed");
  expect(methods).toEqual(["POST", "POST"]);
});
