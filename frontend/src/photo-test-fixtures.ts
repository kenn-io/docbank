import { Blob } from "node:buffer";
import { vi } from "vitest";
import type { PhotoBrowseRow } from "./generated/docbank.js";

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
    camera_make: null, camera_model: null, lens_make: null, lens_model: null, iso: null, exposure_time_seconds: null, f_number: null, exposure_bias_ev: null, focal_length_mm: null, orientation: null,
    previews: { grid: { state: "missing" }, fit: { state: "missing" }, large: { state: "missing" } },
  };
}
