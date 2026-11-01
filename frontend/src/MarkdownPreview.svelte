<script module lang="ts">
  import { renderMarkdown, renderMarkdownSync, type MarkdownRenderer } from "@kenn-io/kit-ui";

  // Kit sanitizes HTML; document content must also remain read-only and must
  // not navigate the application window. Apply that policy before insertion.
  function documentHTML(html: string): string {
    const template = document.createElement("template");
    template.innerHTML = html;
    for (const control of template.content.querySelectorAll(
      "form, input, button, select, textarea",
    ))
      control.remove();
    for (const link of template.content.querySelectorAll("a[href]")) {
      if (link.getAttribute("href")!.startsWith("#")) continue;
      link.setAttribute("target", "_blank");
      link.setAttribute("rel", "noopener noreferrer");
    }
    return template.innerHTML;
  }
  const renderer: MarkdownRenderer = {
    render: async (source) => documentHTML(await renderMarkdown(source)),
    renderSync: (source) => documentHTML(renderMarkdownSync(source)),
  };
</script>

<script lang="ts">
  import { Markdown } from "@kenn-io/kit-ui";
  let { text }: { text: string } = $props();
</script>

<article class="markdown-preview"><Markdown source={text} {renderer} /></article>

<style>
  .markdown-preview {
    width: 100%;
    box-sizing: border-box;
    max-width: 80ch;
    margin: 0 auto;
    padding: var(--space-5);
  }
</style>
