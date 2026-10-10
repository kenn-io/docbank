import { Blob } from "node:buffer";
import { vi } from "vitest";
import type { PhotoBrowseRow, PhotoAlbumSummary } from "./generated/docbank.js";

export function photoAlbum(overrides: Partial<PhotoAlbumSummary> = {}): PhotoAlbumSummary {
  return { id: "11111111-1111-4111-8111-111111111111", name: "Trip", revision: 1, starred: false, created_at: "2025-01-01T00:00:00Z", updated_at: "2025-01-01T00:00:00Z", included_count: 0, member_count: 0, ...overrides };
}

export function albumResponse(body: unknown, status = 200): Response {
  const revision = body && typeof body === "object" && "revision" in body ? body.revision : undefined;
  return new Response(JSON.stringify(body), { status, headers: revision === undefined ? {} : { ETag: `"${revision}"` } });
}

export function storage() {
  vi.stubGlobal("Blob", Blob);
  const data = new Map<string, Map<string, Response>>();
  const open = vi.fn(async (name: string) => {
    const entries = data.get(name) ?? new Map<string, Response>();
    data.set(name, entries);
    return {
      match: vi.fn(async (key: Request) => entries.get(key.url)?.clone()),
      put: vi.fn(async (key: Request, response: Response) => { entries.set(key.url, response.clone()); }),
    };
  });
  const remove = vi.fn(async (name: string) => data.delete(name));
  vi.stubGlobal("caches", { open, delete: remove, keys: vi.fn(async () => [...data.keys()]) });
  return { data, open, remove };
}

export function photo(id: number, capture: string | null = "2025-06-01T12:00:00"): PhotoBrowseRow {
  return {
    asset_id: `photo-${id}`, node_id: id, revision: 1, name: `Photo ${id}.jpg`, kind: "photo",
    capture_time: capture, capture_time_offset: null, capture_time_precision: "second", capture_time_timezone: "omitted",
    content_version_id: `version-${id}`, display_file_id: `file-${id}`, import_time: "2025-06-01T12:00:00Z",
    media_type: "image/jpeg", width_px: 600, height_px: 400,
    previews: { grid: { state: "missing" }, fit: { state: "missing" }, large: { state: "missing" } },
  };
}
