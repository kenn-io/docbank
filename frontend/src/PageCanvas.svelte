<script lang="ts">
  import type { Snippet } from "svelte";
  let {
    url,
    width,
    height,
    zoom = "fit",
    label,
    overlay,
    maxHeight = "480px",
  }: {
    url: string;
    width: number;
    height: number;
    zoom?: string;
    label: string;
    overlay?: Snippet;
    maxHeight?: string;
  } = $props();
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex (keyboard scrolling) -->
<div
  class="page-scroll"
  style:max-height={maxHeight}
  tabindex="0"
  role="region"
  aria-label="Scrollable page image"
>
  <div
    class="page-content"
    style:width={zoom === "page"
      ? `min(100%, calc(${maxHeight} * ${width} / ${height}))`
      : zoom === "fit"
        ? "100%"
        : `${((width / 10000) * 96 * Number(zoom)) / 100}px`}
    style:aspect-ratio={`${width} / ${height}`}
  >
    <img src={url} alt={label} />
    {@render overlay?.()}
  </div>
</div>

<style>
  .page-scroll {
    overflow: auto;
    background: var(--bg-inset);
    min-width: 0;
  }
  .page-content {
    position: relative;
    margin-inline: auto;
    background: white;
  }
  img {
    display: block;
    width: 100%;
    height: 100%;
    position: absolute;
    inset: 0;
  }
</style>
