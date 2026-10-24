<script lang="ts">
  import type { PhotoBrowsePage } from "./generated/docbank.js";

  let {
    status,
    total,
    located = false,
  }: {
    status: Pick<PhotoBrowsePage, "fallback_reason" | "truncated" | "coverage_incomplete">;
    total: number;
    located?: boolean;
  } = $props();
</script>

{#if status.truncated}
  <p class="photo-search-limit" role="status">
    {#if located}
      Top {total.toLocaleString()} located matches. Narrow the search to see others.
    {:else}
      Top {total.toLocaleString()} matches. Counts, Select all, export, and Cull use these photos.
    {/if}
  </p>
{/if}
{#if status.fallback_reason}
  <p class="photo-search-limit" role="status">
    {(
      {
        provider_unavailable: "Image search is unavailable",
        hidden_photos: "Image search is unavailable for hidden photos",
        consent_required: "Image search needs consent",
        index_unavailable: "The image index is unavailable",
        unsupported_query: "No searchable text or unsupported syntax",
      } as Record<string, string>
    )[status.fallback_reason] ?? "Image search is unavailable"}. Showing metadata matches.
  </p>
{:else if status.coverage_incomplete}
  <p class="photo-search-limit" role="status">
    Some photos aren't indexed yet. Showing available image matches.
  </p>
{/if}

<style>
  .photo-search-limit {
    margin: 0;
    padding: var(--space-3) var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
