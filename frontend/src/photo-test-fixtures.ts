import type { PhotoBrowseRow } from "./generated/docbank.js";

export function photo(id: number, capture: string | null = "2025-06-01T12:00:00"): PhotoBrowseRow {
  return {
    asset_id: `photo-${id}`, node_id: id, revision: 1, name: `Photo ${id}.jpg`, kind: "photo",
    capture_time: capture, capture_time_offset: null, capture_time_precision: "second", capture_time_timezone: "omitted",
    content_version_id: `version-${id}`, display_file_id: `file-${id}`, import_time: "2025-06-01T12:00:00Z",
    media_type: "image/jpeg", width_px: 600, height_px: 400,
    previews: { grid: { state: "missing" }, fit: { state: "missing" }, large: { state: "missing" } },
  };
}
