import * as api from "./generated/docbank.js";
import { uuidV4Pattern as uuid } from "./query.js";
import { APIError } from "./api-transport.js";
import { timestamp } from "./collections.js";
import type { Photos } from "./photos.svelte.js";

export const photoDragType = "application/x-docbank-photos";

export type PhotoAlbumItem = api.PhotoAlbum & Partial<api.PhotoAlbumSummary>;

async function readAlbum(response: Response, expectedID?: string): Promise<api.PhotoAlbum> {
  const album = await response.json();
  if (
    !album ||
    typeof album !== "object" ||
    Array.isArray(album) ||
    typeof album.id !== "string" ||
    !uuid.test(album.id) ||
    !Number.isSafeInteger(album.revision) ||
    album.revision < 1 ||
    response.headers.get("ETag") !== `"${album.revision}"` ||
    typeof album.name !== "string" ||
    typeof album.starred !== "boolean" ||
    !timestamp(album.created_at) ||
    !timestamp(album.updated_at) ||
    (album.cover_asset_id != null &&
      (typeof album.cover_asset_id !== "string" || !uuid.test(album.cover_asset_id))) ||
    (album.deleted_at != null && !timestamp(album.deleted_at)) ||
    (expectedID !== undefined && album.id !== expectedID)
  ) {
    throw new Error("Invalid album response.");
  }
  return album;
}

export class PhotoAlbums {
  items = $state<PhotoAlbumItem[]>([]);
  loading = $state(false);
  initialized = $state(false);
  busy = $state(false);
  error = $state("");
  errorCode = $state("");
  unconfirmed = $state<{ name: string }>();
  loadError = $state("");
  notice = $state("");
  noticeID = $state("");
  targetID = $state("");
  drag: api.PhotoAlbumMembersRequest | undefined;
  private controller = new AbortController();
  private generation = 0;
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    private onmemberschange?: (id?: string) => Promise<void>,
  ) {}

  async load() {
    const request = ++this.generation;
    this.loading = true;
    try {
      const items = await api.listPhotoAlbums(this.options());
      if (this.controller.signal.aborted || request !== this.generation) return;
      if (!Array.isArray(items)) throw new Error("Invalid album list response.");
      this.items = items;
      this.initialized = true;
      if (!items.some((album) => album.id === this.targetID)) this.targetID = "";
      this.loadError = "";
      return true;
    } catch (cause) {
      if (!this.controller.signal.aborted && request === this.generation)
        this.loadError = this.failure(cause);
    } finally {
      if (request === this.generation) this.loading = false;
    }
  }

  private options() {
    return { session: this.session, signal: this.controller.signal };
  }
  private failure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }

  rejectBusy() {
    if (!this.busy) return false;
    this.error ||= "Another album change is still running. Try again.";
    return true;
  }

  private async write(
    action: () => Promise<api.PhotoAlbum>,
    creation?: string,
    membership?: { id: string; remove: boolean },
  ) {
    if (this.controller.signal.aborted || this.rejectBusy()) return;
    this.busy = true;
    this.error = "";
    this.errorCode = "";
    this.notice = "";
    this.noticeID = "";
    let result: api.PhotoAlbum | undefined;
    let changed = false;
    try {
      try {
        result = await action();
        changed = true;
        const acknowledged = result;
        const remaining = this.items.filter((album) => album.id !== acknowledged.id);
        this.items = acknowledged.deleted_at ? remaining : [...remaining, acknowledged];
        if (acknowledged.deleted_at && this.targetID === acknowledged.id) this.targetID = "";
      } catch (cause) {
        this.errorCode = cause instanceof APIError ? cause.code : "";
        this.error = this.failure(cause);
        changed = !(cause instanceof APIError && cause.status >= 400 && cause.status < 500);
        if (creation !== undefined && changed) {
          this.unconfirmed = { name: creation };
          this.error = "";
        }
      }
      await this.load();
      if (result) this.error = "";
      if (result && membership) {
        this.notice = `${membership.remove ? "Removed from" : "Added to"} ${result.name}`;
        this.noticeID = membership.id;
      }
    } finally {
      this.busy = false;
    }
    if (membership && changed) void this.onmemberschange?.(membership.id);
    return result;
  }

  async create(name: string, frozen?: { snapshot_id: string; member_hash: string }) {
    if (this.unconfirmed) return;
    const result = await this.write(
      async () =>
        readAlbum(await api.createPhotoAlbum({ name: name.trim(), ...frozen }, this.options())),
      name.trim(),
    );
    if (frozen && (result || this.unconfirmed)) void this.onmemberschange?.(result?.id);
    return result;
  }
  update(album: api.PhotoAlbum, changes: api.UpdatePhotoAlbumRequest) {
    return this.write(async () =>
      readAlbum(
        await api.updatePhotoAlbum(
          album.id,
          changes,
          { "If-Match": `"${album.revision}"` },
          this.options(),
        ),
        album.id,
      ),
    );
  }
  cover(album: api.PhotoAlbum, assetID: string) {
    return this.write(async () =>
      readAlbum(
        await api.setPhotoAlbumCover(
          album.id,
          { asset_id: assetID },
          { "If-Match": `"${album.revision}"` },
          this.options(),
        ),
        album.id,
      ),
    );
  }
  duplicate(album: api.PhotoAlbum, name: string) {
    if (this.unconfirmed) return Promise.resolve(undefined);
    return this.write(
      async () =>
        readAlbum(
          await api.duplicatePhotoAlbum(
            album.id,
            { name: name.trim() },
            { "If-Match": `"${album.revision}"` },
            this.options(),
          ),
        ),
      name.trim(),
    );
  }
  delete(album: api.PhotoAlbum) {
    return this.write(async () =>
      readAlbum(
        await api.deletePhotoAlbum(album.id, { "If-Match": `"${album.revision}"` }, this.options()),
        album.id,
      ),
    );
  }

  members(album: api.PhotoAlbum, scope: api.PhotoAlbumMembersRequest, remove = false) {
    return this.write(
      async () =>
        readAlbum(
          await (remove ? api.removePhotoAlbumMembers : api.addPhotoAlbumMembers)(
            album.id,
            scope,
            { "If-Match": `"${album.revision}"` },
            this.options(),
          ),
          album.id,
        ),
      undefined,
      { id: album.id, remove },
    );
  }

  startDrag(id: string, event: DragEvent, source: Photos) {
    if (!event.dataTransfer) return;
    const selected = source.selectedIDs.has(id);
    this.drag = selected ? source.scope() : { asset_ids: source.stackMembers(id) };
    if (this.drag.asset_ids && this.drag.asset_ids.length > 1000) {
      this.error =
        source.projection === "map"
          ? "Select 1,000 photos or fewer to add to an album."
          : `Select all ${source.total.toLocaleString()} photos to add more than 1,000 at once.`;
      this.drag = undefined;
      event.preventDefault();
      return;
    }
    event.dataTransfer.setData(photoDragType, "photos");
    event.dataTransfer.effectAllowed = "copy";
  }
  async drop(album: api.PhotoAlbum, event: DragEvent) {
    event.preventDefault();
    const drag = this.drag;
    this.drag = undefined;
    if (!event.dataTransfer?.types.includes(photoDragType) || !drag) return;
    await this.members(album, drag);
  }
  dispose() {
    this.controller.abort();
    this.drag = undefined;
  }
}
