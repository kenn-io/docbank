<script module lang="ts">
  export type Tab = { value: string; label: string };
</script>

<script lang="ts">
  import { Tabs } from "@kenn-io/kit-ui";
  import { tick } from "svelte";

  // The app's only tab row: the inspector and the System drawer share it. The
  // selected tab changes color and underline, never weight or width, so the
  // row doesn't shift when you pick one.
  let {
    tabs,
    value,
    ariaLabel,
    onchange,
  }: {
    tabs: Tab[];
    value: string;
    ariaLabel: string;
    onchange: (value: string) => void;
  } = $props();

  function reveal(strip: HTMLElement): void {
    const list = strip.querySelector<HTMLElement>(".kit-tabs__list");
    const active = strip.querySelector<HTMLElement>('[aria-selected="true"]');
    // Narrow screens scroll the row, and each switch can remount it from the left.
    if (list && active && list.scrollWidth > list.clientWidth) list.scrollLeft = active.offsetLeft;
  }

  function change(next: string): void {
    onchange(next);
    // A host that remounts the row on change leaves focus behind, so find the tab again.
    void tick().then(() =>
      Array.from(document.querySelectorAll<HTMLElement>(".kit-tabs__list [data-tab-id]"))
        .find(
          (tab) =>
            tab.dataset.tabId === next &&
            tab.closest("[role=tablist]")?.getAttribute("aria-label") === ariaLabel,
        )
        ?.focus(),
    );
  }
</script>

<div class="tab-strip" {@attach reveal}>
  <Tabs
    tabs={tabs.map((tab) => ({ id: tab.value, label: tab.label }))}
    {value}
    {ariaLabel}
    onchange={change}
  />
</div>

<style>
  .tab-strip {
    min-width: 0;
  }
</style>
