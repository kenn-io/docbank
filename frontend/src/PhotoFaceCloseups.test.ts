import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, screen, waitFor } from "@testing-library/svelte";
import PhotoFaceCloseups from "./PhotoFaceCloseups.svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo } from "./photo-test-fixtures.js";
import type { PhotoFace, PhotoPersonAssertion } from "./generated/docbank.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
it("shows whole-photo marks first, every face by size, and releases crop URLs on navigation", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const cache = new PhotoPreviewCache("session", vi.fn());
  const fetcher = vi
    .spyOn(cache, "getFaceCrop")
    .mockResolvedValue(new Blob(["jpeg"], { type: "image/jpeg" }));
  const created = vi
    .spyOn(URL, "createObjectURL")
    .mockImplementation(() => `blob:crop-${Math.random()}`);
  const revoked = vi.spyOn(URL, "revokeObjectURL");
  const faces = new PhotoFaces("session", vi.fn());
  faces.version = "version-a";
  const base: PhotoFace = {
    face_id: "small",
    content_version_id: "version-a",
    revision: 1,
    box: { x: 0, y: 0, width: 0.001, height: 0.001 },
    created_at: "",
    dismissed: false,
  };
  faces.faces = [base, { ...base, face_id: "large", box: { x: 0, y: 0, width: 0.5, height: 0.5 } }];
  const mark: PhotoPersonAssertion = {
    assertion_id: "mark",
    person_id: "person-a",
    display_name: "Ada",
    content_version_id: "version-a",
    action: "assert",
    role: "depicted",
    photo_mark: true,
    revision: 1,
    recorded_at: "",
    note: "",
  };
  faces.assertions = [mark];
  const selected = {
    ...photo(1),
    content_version_id: "version-a",
    rotation: 90,
    previews: {
      grid: { state: "ready" },
      fit: { state: "ready", generation_id: "fit-a" },
      large: { state: "missing" },
    },
  } as ReturnType<typeof photo>;
  const component = render(PhotoFaceCloseups, { photo: selected, faces, cache });
  await waitFor(() => expect(screen.getAllByRole("img")).toHaveLength(3));
  expect(fetcher.mock.calls.map((call) => call[1])).toEqual([
    { content_version_id: "version-a", generation_id: "fit-a", assertion_id: "mark" },
    { content_version_id: "version-a", generation_id: "fit-a", face_id: "large" },
    { content_version_id: "version-a", generation_id: "fit-a", face_id: "small" },
  ]);
  expect(screen.getAllByRole("img").map((img) => img.getAttribute("alt"))).toEqual([
    "Ada",
    "Unnamed face",
    "Unnamed face",
  ]);
  expect(screen.getAllByRole("img")[0]!.getAttribute("style")).toContain("rotate(90deg)");
  const urls = created.mock.results.map((result) => result.value);
  await component.rerender({
    photo: { ...selected, content_version_id: "version-b" },
    faces,
    cache,
  });
  expect(revoked.mock.calls.map((call) => call[0])).toEqual(urls);
  expect(fetcher.mock.calls.every((call) => call[2]!.aborted)).toBe(true);
  await cache.dispose();
  faces.dispose();
});
