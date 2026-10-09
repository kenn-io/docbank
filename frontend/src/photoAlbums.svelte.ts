import * as api from "./generated/docbank.js";
import { compareUnicodeScalars } from "./query.js";
import { APIError } from "./api-transport.js";
import type { Photos } from "./photos.svelte.js";

export const photoDragType = "application/x-docbank-photos";

export type PhotoAlbumItem = api.PhotoAlbum & Partial<Pick<api.PhotoAlbumSummary, "member_count" | "included_count" | "effective_cover_asset_id" | "cover_generation_id">> & { cover_known?: boolean };

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
  drag: { scope: api.PhotoAlbumMembersRequest; source: Photos; preserve?: Parameters<Photos["refresh"]>[0] } | undefined;
  private controller = new AbortController();
  private read = 0;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, private onmemberschange?: (id: string, source: Photos) => Promise<void>) {}

  load() { return this.busy ? Promise.resolve(undefined) : this.list(); }

  private async list() {
    const read = ++this.read;
    this.loading = true;
    try {
      const items = await api.listPhotoAlbums(this.options());
      if (this.controller.signal.aborted || read !== this.read) return;
      this.items = items.map(album => ({ ...album, cover_known: true }));
      this.initialized = true;
      if (!items.some(album => album.id === this.targetID)) this.targetID = "";
      this.loadError = "";
      return true;
    } catch (cause) { if (!this.controller.signal.aborted && read === this.read) this.loadError = this.failure(cause); }
    finally { if (read === this.read) this.loading = false; }
  }

  private options() { return { session: this.session, signal: this.controller.signal }; }
  private failure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }

  private remember(album: api.PhotoAlbum, empty = false) {
    if (album.deleted_at) {
      this.items = this.items.filter(item => item.id !== album.id);
      if (this.targetID === album.id) this.targetID = "";
      return;
    }
    const existing = this.items.find(item => item.id === album.id);
    const summary: PhotoAlbumItem = { ...existing, ...album, member_count: empty ? 0 : existing?.member_count, included_count: empty ? 0 : existing?.included_count, effective_cover_asset_id: empty ? null : existing?.effective_cover_asset_id, cover_generation_id: empty ? null : existing?.cover_generation_id, cover_known: empty || existing?.cover_known || false };
    this.items = (existing ? this.items.map(item => item.id === album.id ? summary : item) : [...this.items, summary]).sort((a, b) => Number(b.starred) - Number(a.starred) || compareUnicodeScalars(a.name, b.name) || compareUnicodeScalars(a.id, b.id));
  }

  private invalidate(id: string, counts: boolean) {
    this.items = this.items.map(item => item.id === id ? { ...item, ...(counts ? { member_count: undefined, included_count: undefined } : {}), effective_cover_asset_id: undefined, cover_generation_id: undefined, cover_known: false } : item);
  }

  rejectBusy() {
    if (!this.busy) return false;
    this.error ||= "Another album change is still running. Try again.";
    return true;
  }

  private async write(action: () => Promise<api.PhotoAlbum>, id?: string, empty = false, complete?: (result: api.PhotoAlbum | undefined) => Promise<void>) {
    if (this.controller.signal.aborted) return;
    if (this.rejectBusy()) return;
    ++this.read; this.loading = false;
    this.busy = true; this.error = ""; this.notice = ""; this.noticeID = "";
    let result: api.PhotoAlbum | undefined;
    let failure: unknown;
    try {
      try { result = await action(); this.remember(result, empty); }
      catch (cause) { failure = cause; this.error = this.failure(cause); }
      const recovered = await this.list();
      if (recovered && failure instanceof APIError && id && (failure.code === "stale_revision" || failure.status === 404)) {
        const album = this.items.find(item => item.id === id);
        if (!album) this.error = "This album was deleted.";
        else if (failure.code === "stale_revision") this.error = `${album.name} changed elsewhere. Try again.`;
      }
      await complete?.(result);
      return result;
    } finally { this.busy = false; }
  }

  create(name: string) { return this.write(() => api.createPhotoAlbum({ name: name.trim() }, this.options()), undefined, true); }
  update(album: api.PhotoAlbum, changes: api.UpdatePhotoAlbumRequest) {
    return this.write(() => api.updatePhotoAlbum(album.id, changes, { "If-Match": `"${album.revision}"` }, this.options()), album.id);
  }
  cover(album: api.PhotoAlbum, assetID: string) {
    return this.write(() => { this.invalidate(album.id, false); return api.setPhotoAlbumCover(album.id, { asset_id: assetID }, { "If-Match": `"${album.revision}"` }, this.options()); }, album.id);
  }
  duplicate(album: api.PhotoAlbum, name: string) {
    return this.write(() => api.duplicatePhotoAlbum(album.id, { name: name.trim() }, { "If-Match": `"${album.revision}"` }, this.options()), album.id);
  }
  delete(album: api.PhotoAlbum) {
    return this.write(() => api.deletePhotoAlbum(album.id, { "If-Match": `"${album.revision}"` }, this.options()), album.id);
  }

  async members(album: api.PhotoAlbum, scope: api.PhotoAlbumMembersRequest, source: Photos, remove = false, preserve?: Parameters<Photos["refresh"]>[0]) {
    const ids = scope.asset_ids ? [...scope.asset_ids] : undefined;
    const batches = ids ? Array.from({ length: Math.ceil(ids.length / 1000) }, (_, index) => ({ asset_ids: ids.slice(index * 1000, (index + 1) * 1000) })) : [scope];
    if (!batches.length) return;
    let completed = 0;
    let missingPhoto = false;
    return this.write(async () => {
      let revision = album.revision;
      let result: api.PhotoAlbum = album;
      for (const batch of batches) {
        this.invalidate(album.id, true);
        try { result = await (remove ? api.removePhotoAlbumMembers : api.addPhotoAlbumMembers)(album.id, batch, { "If-Match": `"${revision}"` }, this.options()); }
        catch (cause) { missingPhoto = cause instanceof APIError && cause.status === 404; throw cause; }
        if (batch !== batches[batches.length - 1]) this.remember(result);
        revision = result.revision;
        completed += batch.asset_ids?.length ?? 0;
      }
      return result;
    }, album.id, false, async result => {
      if (completed && ids && (!result || remove)) {
        const finished = new Set(ids.slice(0, completed));
        source.selection = { ...source.selection, selectedIDs: new Set([...source.selection.selectedIDs].filter(id => !finished.has(id))) };
        if (!result) this.error = `${remove ? "Removed" : "Added"} ${completed.toLocaleString()} of ${ids.length.toLocaleString()} photos. ${this.error}`;
      }
      if (result) {
        const refreshed = this.loadError ? undefined : this.items.find(item => item.id === album.id);
        this.notice = `${remove ? "Removed from" : "Added to"} ${album.name}${refreshed?.included_count !== undefined ? ` · now ${refreshed.included_count.toLocaleString()} ${refreshed.included_count === 1 ? "photo" : "photos"}` : ""}`;
        this.noticeID = album.id;
      }
      if (source.query.filters?.set_ids?.includes(album.id) || missingPhoto && !this.loadError && this.items.some(item => item.id === album.id)) await source.refresh(preserve);
      await this.onmemberschange?.(album.id, source);
    });
  }

  startDrag(id: string, event: DragEvent, source: Photos, preserve?: Parameters<Photos["refresh"]>[0]) {
    if (!event.dataTransfer) return;
    const selected = source.selection.selectedIDs.has(id);
    this.drag = { source, preserve, scope: selected ? source.scope() : { asset_ids: [id] } };
    event.dataTransfer.setData(photoDragType, "photos");
    event.dataTransfer.effectAllowed = "copy";
  }
  async drop(album: api.PhotoAlbum, event: DragEvent) {
    event.preventDefault();
    const drag = this.drag; this.drag = undefined;
    if (!event.dataTransfer?.types.includes(photoDragType) || !drag) return;
    await this.members(album, drag.scope, drag.source, false, drag.preserve);
  }
  dispose() { this.controller.abort(); this.drag = undefined; }
}
