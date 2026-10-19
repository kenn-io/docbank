import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import {
  photoImportManifest,
  photoImportStorage,
  type PhotoImportCheckpoint,
} from "./photoImportStorage.js";
import { photoSelectionFiles, photoSelectionLastModified } from "./photo-test-fixtures.js";

beforeEach(() => vi.restoreAllMocks());
afterEach(() => vi.unstubAllGlobals());

it("builds canonical photo manifests and reports hashing progress", async () => {
  const canonicalFiles = photoSelectionFiles();
  const first = await photoImportManifest(canonicalFiles, new AbortController().signal);
  const reordered = await photoImportManifest(
    [...canonicalFiles].reverse(),
    new AbortController().signal,
  );
  expect(first.digest).toBe("ea7b9e6368be530a55fadb486158f9b6b5c01fb4b0bb1c6c995d18475c1acb85");
  expect(reordered.digest).toBe(first.digest);
  for (const changed of [
    new File(["RAW"], canonicalFiles[0]!.name, { lastModified: canonicalFiles[0]!.lastModified }),
    new File(["raw"], "other.ARW", { lastModified: canonicalFiles[0]!.lastModified }),
    new File(["raw"], canonicalFiles[0]!.name, {
      lastModified: canonicalFiles[0]!.lastModified + 1,
    }),
    new File(["raw!"], canonicalFiles[0]!.name, { lastModified: canonicalFiles[0]!.lastModified }),
  ]) {
    const result = await photoImportManifest(
      [changed, ...canonicalFiles.slice(1)],
      new AbortController().signal,
    );
    expect(result.digest).not.toBe(first.digest);
  }

  const files = [
    ...photoSelectionFiles(),
    new File(["xmp"], "capture.xmp", { lastModified: photoSelectionLastModified }),
  ];
  for (const file of files)
    Object.defineProperty(file, "webkitRelativePath", { value: `Trip/${file.name}` });
  const progress = vi.fn();
  const reads = files.map((file) => vi.spyOn(file, "slice"));
  const { manifest } = await photoImportManifest(files, new AbortController().signal, progress);
  expect(manifest).toEqual([
    {
      path: "Trip/capture.ARW",
      size: 3,
      modified: photoSelectionLastModified,
      hash: "d7439bee24773bcbfa2d0a97947ee36227b10d1022b1a55847e928965bb6bfde",
    },
    {
      path: "Trip/capture.JPG",
      size: 4,
      modified: photoSelectionLastModified,
      hash: "41e5787e9f28562d07b891b1816b492309d646c0f2829743fa4963a9f9cc1d61",
    },
    {
      path: "Trip/capture.xmp",
      size: 3,
      modified: photoSelectionLastModified,
      hash: "faf36afdfbef88fbc5567b876c57be7431d3cf27f6a2209177db13d331d82752",
    },
    {
      path: "Trip/movie.MP4",
      size: 5,
      modified: photoSelectionLastModified,
      hash: "0cab1c9617404faf2b24e221e189ca5945813e14d3f766345b09ca13bbe28ffc",
    },
    {
      path: "Trip/notes.txt",
      size: 4,
      modified: photoSelectionLastModified,
      hash: null,
    },
  ]);
  expect(progress).toHaveBeenLastCalledWith({ processed: 15, total: 15 });
  expect(reads.map((read) => read.mock.calls.length)).toEqual([1, 1, 1, 0, 1]);
});

const checkpoint: PhotoImportCheckpoint = {
  sourceID: "e1f7a31e-6b61-4aba-a660-360de07064db",
  revision: "17a02b95-45f7-4c13-b146-bf3df59e458b",
  destination: "/photos",
  manifestVersion: 2,
  manifestHash: "a".repeat(64),
  pickerKind: "folder",
};

it("confirms the persistent cookie before saving the selection digest with session credentials", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ ready: false }))
    .mockResolvedValueOnce(Response.json({ ready: true }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  await photoImportStorage("vault", "tab-session").replace(checkpoint);
  expect(fetch.mock.calls.map(([url]) => url)).toEqual([
    "/api/daemon/photo-import-checkpoint/header",
    "/api/daemon/photo-import-checkpoint/header",
    "/api/daemon/photo-import-checkpoint",
  ]);
  for (const [, options] of fetch.mock.calls) {
    expect(options.headers.get("X-Docbank-Web-Session")).toBe("tab-session");
    expect(options.credentials).toBe("same-origin");
  }
  expect(JSON.parse(fetch.mock.calls[2]![1].body)).toEqual({ next: checkpoint, expected: null });
});

it("refuses to save if the profile cookie does not return", async () => {
  const fetch = vi.fn().mockImplementation(async () => Response.json({ ready: false }));
  vi.stubGlobal("fetch", fetch);
  await expect(photoImportStorage("vault", "session").replace(checkpoint)).rejects.toThrow(
    "Allow cookies",
  );
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("loads the saved digest and picker kind and sends only the expected identity when clearing it", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ ready: true }))
    .mockResolvedValueOnce(Response.json({ checkpoint }))
    .mockResolvedValueOnce(Response.json({ ready: true }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  const storage = photoImportStorage("vault", "session");
  expect(await storage.load()).toEqual(checkpoint);
  await storage.replace(undefined, checkpoint);
  expect(JSON.parse(fetch.mock.calls[3]![1].body)).toEqual({
    next: null,
    expected: { sourceID: checkpoint.sourceID, revision: checkpoint.revision },
  });
});
