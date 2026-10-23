import { type PhotoBrowseRow } from "./photoRows.js";
import { Blob } from "node:buffer";
import { vi } from "vite-plus/test";
import { fireEvent } from "@testing-library/svelte";
import type {
  PhotoMapRow,
  PhotoAlbumSummary,
  PhotoPreparedTarget,
  PhotoAuthoredPreparation,
} from "./generated/docbank.js";
import type { Query } from "./query.js";
import type { SavedQuery } from "./savedQueries.js";

type SmartSetOverrides = Partial<Omit<SavedQuery, "kind" | "payload" | "fingerprint">>;

export function savedSmartSet(
  payload: Query,
  fingerprint: string,
  overrides: SmartSetOverrides = {},
): Extract<SavedQuery, { kind: "query" }> {
  return {
    id: "00000000-0000-4000-8000-000000000001",
    name: "Five stars",
    description: "",
    kind: "query",
    payload,
    fingerprint,
    revision: 1,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

export async function touchTap(
  target: HTMLElement,
  time: number,
  detail = 1,
  clickPointerType?: string,
  point = { x: 0, y: 0 },
): Promise<void> {
  await fireEvent.pointerDown(target, { pointerType: "touch" });
  await fireEvent.pointerUp(target, { pointerType: "touch" });
  const event = new MouseEvent("click", {
    bubbles: true,
    button: 0,
    detail,
    clientX: point.x,
    clientY: point.y,
  });
  Object.defineProperty(event, "timeStamp", { value: time });
  if (clickPointerType !== undefined)
    Object.defineProperty(event, "pointerType", { value: clickPointerType });
  await fireEvent(target, event);
}

export function photoAlbum(overrides: Partial<PhotoAlbumSummary> = {}): PhotoAlbumSummary {
  return {
    id: "11111111-1111-4111-8111-111111111111",
    name: "Trip",
    revision: 1,
    starred: false,
    created_at: "2025-01-01T00:00:00Z",
    updated_at: "2025-01-01T00:00:00Z",
    included_count: 0,
    member_count: 0,
    cover_rotation: 0,
    ...overrides,
  };
}

export function albumResponse(body: unknown, status = 200): Response {
  const revision =
    body && typeof body === "object" && "revision" in body ? body.revision : undefined;
  return new Response(JSON.stringify(body), {
    status,
    headers: revision === undefined ? {} : { ETag: `"${revision}"` },
  });
}

export function storage() {
  vi.stubGlobal("Blob", Blob);
  const data = new Map<string, Map<string, Response>>();
  const open = vi.fn(async (name: string) => {
    const entries = data.get(name) ?? new Map<string, Response>();
    data.set(name, entries);
    return {
      match: vi.fn(async (key: Request) => entries.get(key.url)?.clone()),
      put: vi.fn(async (key: Request, response: Response) => {
        entries.set(key.url, response.clone());
      }),
    };
  });
  const remove = vi.fn(async (name: string) => data.delete(name));
  vi.stubGlobal("caches", { open, delete: remove, keys: vi.fn(async () => [...data.keys()]) });
  return { data, open, remove };
}

export function photo(id: number, capture: string | null = "2025-06-01T12:00:00"): PhotoBrowseRow {
  return {
    rotation: 0,
    rating: 0,
    flag: "",
    asset_id: `photo-${id}`,
    node_id: id,
    revision: 1,
    name: `Photo ${id}.jpg`,
    kind: "photo",
    capture_time: capture,
    file_modified_time: null,
    capture_time_offset: null,
    capture_time_precision: "second",
    capture_time_timezone: "omitted",
    content_version_id: `version-${id}`,
    display_file_id: `file-${id}`,
    import_time: "2025-06-01T12:00:00Z",
    media_type: "image/jpeg",
    width_px: 600,
    height_px: 400,
    camera_make: null,
    camera_model: null,
    lens_make: null,
    lens_model: null,
    iso: null,
    exposure_time_seconds: null,
    f_number: null,
    exposure_bias_ev: null,
    focal_length_mm: null,
    orientation: null,
    previews: {
      grid: { state: "missing" },
      fit: { state: "missing" },
      large: { state: "missing" },
    },
  };
}

export function preparedPhoto(
  id: number,
  overrides: Partial<PhotoPreparedTarget> = {},
): PhotoPreparedTarget {
  return {
    current_content_version_id: `version-${id}`,
    asset_id: `photo-${id}`,
    asset_revision: 1,
    display_file_id: `file-${id}`,
    file_id: `file-${id}`,
    file_name: `Photo ${id}.jpg`,
    node_id: id,
    revision: 1,
    node_revision: 1,
    tags: [],
    values: {
      flag_owner: "",
      rating: 0,
      flag: "",
      label: "",
      caption: "",
      creator: "",
      copyright: "",
      rotation: 0,
    },
    ...overrides,
  };
}

export function photoPreparation(targets: PhotoPreparedTarget[]): PhotoAuthoredPreparation {
  return {
    targets,
    assets: [...new Set(targets.map((target) => target.asset_id))].map((id) => ({
      id,
      revision: 1,
      kind: "photo",
      agreement: {},
      display_source: "default",
      display_file_id: targets.find((target) => target.asset_id === id)!.file_id,
      created_at: "2025-01-01T00:00:00Z",
      updated_at: "2025-01-01T00:00:00Z",
      files: targets
        .filter((target) => target.asset_id === id)
        .map((target) => ({
          ...target.values,
          id: target.file_id,
          asset_id: id,
          node_id: target.node_id,
          revision: target.revision,
          role: "image",
          created_at: "2025-01-01T00:00:00Z",
        })),
    })),
  };
}
export function photoMetadataResponse(
  url: string,
  init?: RequestInit,
): Promise<Response> | undefined {
  if (url.endsWith("/photos/authored/prepare")) {
    const scope = JSON.parse(String(init?.body ?? "{}"));
    const ids: string[] = scope.asset_ids ?? [];
    const targets = ids.map((id) => preparedPhoto(Number(id.slice("photo-".length))));
    return Promise.resolve(Response.json(photoPreparation(targets)));
  }
}

export function mapPhoto(id: number): PhotoMapRow {
  const {
    asset_id,
    display_file_id,
    revision,
    name,
    capture_time,
    file_modified_time,
    width_px,
    height_px,
    previews,
    rotation,
  } = photo(id);
  return {
    asset_id,
    display_file_id,
    revision,
    name,
    capture_time,
    file_modified_time,
    width_px,
    height_px,
    rotation,
    latitude: 0,
    longitude: 12,
    previews: { grid: previews.grid },
  };
}

// photoSelectionLastModified is a fixed last-modified time (ms since epoch)
// so tests asserting on the wire-sent modified_time field get a stable value
// instead of each File defaulting to the time it was constructed.
export const photoSelectionLastModified = 1700000000000;

export const photoSelectionFiles = () => [
  new File(["raw"], "capture.ARW", { lastModified: photoSelectionLastModified }),
  new File(["jpeg"], "capture.JPG", { lastModified: photoSelectionLastModified }),
  new File(["video"], "movie.MP4", { lastModified: photoSelectionLastModified }),
  new File(["text"], "notes.txt", { lastModified: photoSelectionLastModified }),
];
