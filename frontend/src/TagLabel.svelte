<script lang="ts">
  import type { ColorLabelSize } from "@kenn-io/kit-ui";
  import type { Tag } from "./generated/docbank.js";
  import { presentTag } from "./tagPresentation.js";

  let {
    tag,
    size = "md",
    title,
  }: {
    tag: Tag;
    size?: ColorLabelSize;
    title?: string;
  } = $props();

  const presented = $derived(presentTag(tag));
</script>

<span
  class="tag-label tag-label--{size}"
  title={title ?? tag.name}
>
  <span class="tag-label__visual" aria-hidden="true">
    <span class="tag-label__dot" style:background={presented.color}></span><span
      class="tag-label__name">{presented.label}</span>
  </span>
  <span class="kit-sr-only">{tag.name}</span>
</span>

<style>
  .tag-label {
    display: inline-flex;
    min-width: 0;
    max-width: 100%;
  }

  .tag-label__visual {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
    max-width: 100%;
    padding: 2px var(--space-4) 2px var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: 999px;
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.3;
  }

  .tag-label--sm .tag-label__visual {
    font-size: var(--font-size-xs);
  }

  .tag-label__dot {
    flex: 0 0 auto;
    width: 8px;
    height: 8px;
    border-radius: 50%;
  }

  .tag-label__name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
