<script lang="ts">
  import { setContext, type Snippet } from "svelte";
  import { drawerPage } from "./drawerNavigation.js";

  let { title, children }: { title: string; children: Snippet } = $props();
  setContext(drawerPage, true);
</script>

<main class="tool-page" aria-label={title}>
  {@render children()}
</main>

<style>
  .tool-page {
    flex: 1;
    min-height: 0;
    overflow: auto;
    background: var(--bg-surface);
  }
</style>
