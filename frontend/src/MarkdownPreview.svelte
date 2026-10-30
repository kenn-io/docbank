<script lang="ts">
  import { Markdown } from "@kenn-io/kit-ui";
  let { text }: { text: string } = $props();
</script>

<article class="markdown-preview"><Markdown source={text} /></article>

<style>
  .markdown-preview {
    width: 100%;
    box-sizing: border-box;
    max-width: 80ch;
    margin: 0 auto;
    padding: var(--space-5);
  }
</style>
