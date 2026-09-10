<script lang="ts">
  import { ColorLabel, type ColorLabelSize } from "@kenn-io/kit-ui";
  import type { Tag } from "./api.js";
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
  class="tag-label"
  title={title ?? tag.name}
>
  <span class="tag-label__visual" aria-hidden="true">
    <ColorLabel
      name={presented.label}
      color={presented.color}
      {size}
    />
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
    min-width: 0;
    max-width: 100%;
  }
</style>
