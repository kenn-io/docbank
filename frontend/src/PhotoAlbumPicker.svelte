<script lang="ts">
  import { onDestroy } from "svelte";
  import { KbdBadge, Typeahead } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import { type PhotoAlbums } from "./photoAlbums.svelte.js";
  import type { PhotoAlbum, PhotoAlbumMembersRequest } from "./generated/docbank.js";
  let { photos, albums, preserve }: { photos: Photos; albums: PhotoAlbums; preserve?: Parameters<Photos["refresh"]>[0] } = $props();
  let alive = true;
  onDestroy(() => alive = false);
  let error = $state("");
  let element = $state<HTMLDivElement>();
  const target = $derived(albums.items.find(album => album.id === albums.targetID));
  // Album names cannot contain NUL, so option keys cannot collide with valid names.
  const choiceKey = (id: string) => `\0${id}`;

  export function focus() { if (alive) element?.querySelector<HTMLButtonElement>("button")?.click(); }
  async function add(album: PhotoAlbum | undefined, scope: PhotoAlbumMembersRequest) {
    error = "";
    if (!album) { error = "This album is no longer available. Choose another album."; return false; }
    const result = await albums.members(album, scope, photos, false, preserve);
    if (!result && alive) { error = albums.error; albums.error = ""; }
    return !!result;
  }
  export async function addToTarget() { const result = await add(target, photos.scope()); if (!result) focus(); return result; }
  async function choose(value: string) {
    error = "";
    const scope = photos.scope();
    let album: PhotoAlbum | undefined = albums.items.find(item => choiceKey(item.id) === value);
    if (!album) {
      album = await albums.create(value);
      if (!album) { if (alive) { error = albums.error; albums.error = ""; } return false; }
    }
    albums.targetID = album.id;
    return add(album, scope);
  }
</script>

<div class="album-picker" bind:this={element}>
  <Typeahead options={albums.items.map(album => ({ name: choiceKey(album.id), label: album.name }))} value={albums.targetID ? choiceKey(albums.targetID) : ""} fallbackLabel="Add to album" triggerPrefix={target ? "Add to album · " : ""} placeholder="Find or create an album" title="Add to album" allowCustom customLabel={'Create album "{query}"'} placement="top" {error} loading={albums.loading || albums.busy} onselect={choose} />
  {#if target}<span class="target-hint"><KbdBadge keys={['B']} /> adds to {target.name}</span>{/if}
</div>

<style>
  .album-picker { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); --typeahead-panel-min-width: 240px; }
  .target-hint { display: flex; gap: 6px; align-items: center; color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
