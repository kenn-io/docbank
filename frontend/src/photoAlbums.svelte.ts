import * as api from "./generated/docbank.js";
import { uuidV4Pattern as uuid } from "./query.js";
import { APIError } from "./api-transport.js";
import type { Photos } from "./photos.svelte.js";

export const photoDragType = "application/x-docbank-photos";

export type PhotoAlbumItem = api.PhotoAlbumSummary;

const timestamp = (value: unknown) => typeof value === "string" && Number.isFinite(Date.parse(value));

async function readAlbum(response: Response, expectedID?: string): Promise<api.PhotoAlbum> {
  const album = await response.json();
  if (!album || typeof album !== "object" || Array.isArray(album) ||
      typeof album.id !== "string" || !uuid.test(album.id) ||
      !Number.isSafeInteger(album.revision) || album.revision < 1 ||
      typeof album.name !== "string" || typeof album.starred !== "boolean" ||
      !timestamp(album.created_at) || !timestamp(album.updated_at) ||
      album.cover_asset_id != null && (typeof album.cover_asset_id !== "string" || !uuid.test(album.cover_asset_id)) ||
      album.deleted_at != null && !timestamp(album.deleted_at) ||
      expectedID !== undefined && album.id !== expectedID) {
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
  loadError = $state("");
  notice = $state("");
  noticeID = $state("");
  targetID = $state("");
  drag: api.PhotoAlbumMembersRequest | undefined;
  private controller = new AbortController();
  private generation = 0;
  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {}

  async load() {
    const request = ++this.generation;
    this.loading = true;
    try {
      const items = await api.listPhotoAlbums(this.options());
      if (this.controller.signal.aborted || request !== this.generation) return;
      this.items = items;
      this.initialized = true;
      if (!items.some(album => album.id === this.targetID)) this.targetID = "";
      this.loadError = "";
      return true;
    } catch (cause) { if (!this.controller.signal.aborted && request === this.generation) this.loadError = this.failure(cause); }
    finally { if (request === this.generation) this.loading = false; }
  }

  private options() { return { session: this.session, signal: this.controller.signal }; }
  private failure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }

  rejectBusy() {
    if (!this.busy) return false;
    this.error ||= "Another album change is still running. Try again.";
    return true;
  }

  private async write(action: () => Promise<api.PhotoAlbum>) {
    if (this.controller.signal.aborted || this.rejectBusy()) return;
    this.busy = true; this.error = ""; this.notice = ""; this.noticeID = "";
    let result: api.PhotoAlbum | undefined;
    try {
      try { result = await action(); }
      catch (cause) { this.error = this.failure(cause); }
      await this.load();
      if (result) this.error = "";
      return result;
    } finally { this.busy = false; }
  }

  create(name: string) {
    return this.write(async () => readAlbum(await api.createPhotoAlbum({ name: name.trim() }, this.options())));
  }
  update(album: api.PhotoAlbum, changes: api.UpdatePhotoAlbumRequest) {
    return this.write(async () => readAlbum(await api.updatePhotoAlbum(album.id, changes, { "If-Match": `"${album.revision}"` }, this.options()), album.id));
  }
  cover(album: api.PhotoAlbum, assetID: string) {
    return this.write(async () => readAlbum(await api.setPhotoAlbumCover(album.id, { asset_id: assetID }, { "If-Match": `"${album.revision}"` }, this.options()), album.id));
  }
  duplicate(album: api.PhotoAlbum, name: string) {
    return this.write(async () => readAlbum(await api.duplicatePhotoAlbum(album.id, { name: name.trim() }, { "If-Match": `"${album.revision}"` }, this.options())));
  }
  delete(album: api.PhotoAlbum) {
    return this.write(async () => readAlbum(await api.deletePhotoAlbum(album.id, { "If-Match": `"${album.revision}"` }, this.options()), album.id));
  }

  async members(album: api.PhotoAlbum, scope: api.PhotoAlbumMembersRequest, remove = false) {
    const result = await this.write(async () => readAlbum(await (remove ? api.removePhotoAlbumMembers : api.addPhotoAlbumMembers)(album.id, scope, { "If-Match": `"${album.revision}"` }, this.options()), album.id));
    if (result) {
      const refreshed = this.loadError ? undefined : this.items.find(item => item.id === album.id);
      this.notice = `${remove ? "Removed from" : "Added to"} ${album.name}${refreshed?.included_count !== undefined ? ` · now ${refreshed.included_count.toLocaleString()} ${refreshed.included_count === 1 ? "photo" : "photos"}` : ""}`;
      this.noticeID = album.id;
    }
    return result;
  }

  startDrag(id: string, event: DragEvent, source: Photos) {
    if (!event.dataTransfer) return;
    const selected = source.selection.selectedIDs.has(id);
    this.drag = selected ? source.scope() : { asset_ids: [id] };
    event.dataTransfer.setData(photoDragType, "photos");
    event.dataTransfer.effectAllowed = "copy";
  }
  async drop(album: api.PhotoAlbum, event: DragEvent) {
    event.preventDefault();
    const drag = this.drag; this.drag = undefined;
    if (!event.dataTransfer?.types.includes(photoDragType) || !drag) return;
    await this.members(album, drag);
  }
  dispose() { this.controller.abort(); this.drag = undefined; }
}
