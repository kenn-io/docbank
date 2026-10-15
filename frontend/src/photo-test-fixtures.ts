import { Blob } from "node:buffer";
import { vi } from "vite-plus/test";
import { fireEvent } from "@testing-library/svelte";
import type {
  PhotoBrowseRow,
  PhotoMapRow,
  PhotoAlbumSummary,
  PhotoPreparedTarget,
} from "./generated/docbank.js";

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
    asset_id: `photo-${id}`,
    node_id: id,
    revision: 1,
    name: `Photo ${id}.jpg`,
    kind: "photo",
    capture_time: capture,
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
    asset_id: `photo-${id}`,
    asset_revision: 1,
    file_id: `file-${id}`,
    file_name: `Photo ${id}.jpg`,
    node_id: id,
    revision: 1,
    node_revision: 1,
    tags: [],
    values: {
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

export function photoMetadataResponse(
  url: string,
  init?: RequestInit,
): Promise<Response> | undefined {
  if (url.endsWith("/photos/authored/prepare")) {
    const scope = JSON.parse(String(init?.body ?? "{}"));
    const ids: string[] = scope.asset_ids ?? [];
    const targets = ids.map((id) => preparedPhoto(Number(id.slice("photo-".length))));
    return Promise.resolve(
      Response.json({
        assets: targets.map((target) => ({
          id: target.asset_id,
          files: [
            {
              id: target.file_id,
              node_id: target.node_id,
              revision: target.revision,
              role: "image",
            },
          ],
        })),
        targets,
      }),
    );
  }
}

export function mapPhoto(id: number): PhotoMapRow {
  const { asset_id, revision, name, capture_time, width_px, height_px, previews, rotation } =
    photo(id);
  return {
    asset_id,
    revision,
    name,
    capture_time,
    width_px,
    height_px,
    rotation,
    latitude: 0,
    longitude: 12,
    previews: { grid: previews.grid },
  };
}
