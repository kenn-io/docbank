<script lang="ts">
  import { Chip } from "@kenn-io/kit-ui";
  import FileTypeIcon from "./FileTypeIcon.svelte";
  import { roleChipLabel, type MatchedRole } from "./personRoles.js";

  // A result's name with the folder it lives in as one muted line beneath, so a
  // long path never wraps or repeats the name. The folder keeps its end, which
  // is the part that tells siblings apart. Chips show frozen roles held by
  // positively named people and text mentions, beside or beneath the name.
  let {
    name,
    folder = "",
    mime,
    excerpt = "",
    roles = [],
  }: {
    name: string;
    folder?: string;
    mime?: string;
    excerpt?: string;
    roles?: readonly MatchedRole[];
  } = $props();
</script>

<span class="result-name">
  <FileTypeIcon kind="file" {mime} />
  <span class="text">
    {#if roles.length}<span class="title-line"
        ><span class="title">{name}</span><span class="roles"
          >{#each roles as role (role)}<Chip size="xs" tone="neutral" uppercase={false}
              >{roleChipLabel(role)}</Chip
            >{/each}</span
        ></span
      >{:else}<span class="title">{name}</span>{/if}
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
    min-width: 0;
  }
  /* On small screens, let chips wrap beneath the name. */
  .title-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    column-gap: var(--space-2);
    row-gap: var(--space-1);
    min-width: 0;
  }
  .roles {
    display: flex;
    flex: 0 1 auto;
    flex-wrap: wrap;
    gap: var(--space-1);
    min-width: 0;
  }
  @media (min-width: 641px) {
    .title-line {
      flex-wrap: nowrap;
    }
    .roles {
      flex: none;
      flex-wrap: nowrap;
    }
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
