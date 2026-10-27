<script lang="ts">
  import FileTypeIcon from "./FileTypeIcon.svelte";

  // A result's name with the folder it lives in as one muted line beneath, so a
  // long path never wraps or repeats the name. The folder keeps its end, which
  // is the part that tells siblings apart.
  let {
    name,
    folder = "",
    mime,
    excerpt = "",
  }: { name: string; folder?: string; mime?: string; excerpt?: string } = $props();
</script>

<span class="result-name">
  <FileTypeIcon kind="file" {mime} />
  <span class="text">
    <span class="title">{name}</span>
    {#if folder}<span class="folder"><bdi>{folder}</bdi></span>{/if}
    {#if excerpt}<span class="excerpt">{excerpt}</span>{/if}
  </span>
</span>

<style>
  .result-name {
    display: flex;
    align-items: center;
    gap: var(--space-5);
    min-width: 0;
  }
  .text {
    display: grid;
    min-width: 0;
  }
  .title,
  .folder,
  .excerpt {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .title {
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
  }
  .folder,
  .excerpt {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  /* Right-to-left clips the start of the line, so the ellipsis eats the root. */
  .folder {
    direction: rtl;
    text-align: left;
  }
  .folder bdi {
    direction: ltr;
  }
</style>
