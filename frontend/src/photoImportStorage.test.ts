import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import {
  photoImportManifest,
  photoImportStorage,
  type PhotoImportCheckpoint,
} from "./photoImportStorage.js";
import { photoSelectionFiles, photoSelectionLastModified } from "./photo-test-fixtures.js";

beforeEach(() => vi.restoreAllMocks());
afterEach(() => vi.unstubAllGlobals());

it("builds canonical photo manifests without reading any file", async () => {
  const canonicalFiles = photoSelectionFiles();
  const reads = canonicalFiles.map((file) => vi.spyOn(file, "slice"));
  const first = photoImportManifest(canonicalFiles);
  const reordered = photoImportManifest([...canonicalFiles].reverse());
  expect(first.digest).toBe(reordered.digest);
  expect(first.digest).toBe("79c254592043792e6addb3183a54db68a315a06da59c57c4ce0cbedfc3833d71");
  for (const changed of [
    new File(["raw"], "other.ARW", { lastModified: canonicalFiles[0]!.lastModified }),
    new File(["raw"], canonicalFiles[0]!.name, {
      lastModified: canonicalFiles[0]!.lastModified + 1,
    }),
    new File(["raw!"], canonicalFiles[0]!.name, { lastModified: canonicalFiles[0]!.lastModified }),
  ]) {
    expect(photoImportManifest([changed, ...canonicalFiles.slice(1)]).digest).not.toBe(
      first.digest,
    );
  }
  expect(reads.map((read) => read.mock.calls.length)).toEqual([0, 0, 0, 0]);

  const files = photoSelectionFiles();
  for (const file of files)
    Object.defineProperty(file, "webkitRelativePath", { value: `Trip/${file.name}` });
  expect(photoImportManifest(files.reverse()).manifest).toEqual([
    { path: "Trip/capture.ARW", size: 3, modified: photoSelectionLastModified },
    { path: "Trip/capture.JPG", size: 4, modified: photoSelectionLastModified },
    { path: "Trip/movie.MP4", size: 5, modified: photoSelectionLastModified },
    { path: "Trip/notes.txt", size: 4, modified: photoSelectionLastModified },
  ]);
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
