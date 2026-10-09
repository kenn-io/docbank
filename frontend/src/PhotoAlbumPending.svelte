<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type { PhotoAlbums } from "./photoAlbums.svelte.js";
  let { albums, onnavigate }: { albums: PhotoAlbums; onnavigate: (path: string) => void } = $props();
</script>

{#if albums.unconfirmed}
  <div class="album-pending" role="alert">
    <span>An album named "{albums.unconfirmed.name}" may already exist. Another attempt can create an extra album.</span>
    <div class="pending-actions"><Button size="sm" onclick={() => onnavigate("/photos/albums")}>View Albums</Button><Button size="sm" disabled={albums.busy || albums.loading} onclick={() => void albums.load()}>Refresh albums</Button><Button size="sm" disabled={albums.busy} onclick={() => albums.unconfirmed = undefined}>Allow another album</Button></div>
  </div>
{/if}

<style>
  .album-pending { padding: var(--space-4); background: var(--bg-inset); }
  .pending-actions { display: flex; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-3); }
</style>
