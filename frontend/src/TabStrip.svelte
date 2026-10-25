<script module lang="ts">
  export type Tab = { value: string; label: string };
</script>

<script lang="ts">
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
    const active = strip.querySelector<HTMLElement>('[aria-selected="true"]');
    // Narrow screens scroll the row, and each switch can remount it from the left.
    if (active && strip.scrollWidth > strip.clientWidth) strip.scrollLeft = active.offsetLeft;
  }

  function move(event: KeyboardEvent, index: number): void {
    const step = event.key === "ArrowRight" ? 1 : event.key === "ArrowLeft" ? -1 : 0;
    const edge = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : undefined;
    if (!step && edge === undefined) return;
    event.preventDefault();
    const next = tabs[edge ?? (index + step + tabs.length) % tabs.length];
    if (!next) return;
    onchange(next.value);
    // A host that remounts the row on change leaves focus behind, so find the tab again.
    void tick().then(() =>
      Array.from(document.querySelectorAll<HTMLElement>(`[role="tablist"] [data-tab]`))
        .find(
          (tab) =>
            tab.dataset.tab === next.value &&
            tab.closest("[role=tablist]")?.getAttribute("aria-label") === ariaLabel,
        )
        ?.focus(),
    );
  }
</script>

<div class="tab-strip" role="tablist" aria-label={ariaLabel} {@attach reveal}>
  {#each tabs as tab, index (tab.value)}
    <button
      type="button"
      role="tab"
      data-tab={tab.value}
      aria-selected={tab.value === value}
      tabindex={tab.value === value ? 0 : -1}
      onclick={() => onchange(tab.value)}
      onkeydown={(event) => move(event, index)}>{tab.label}</button
    >
  {/each}
</div>

<style>
  .tab-strip {
    display: flex;
    gap: var(--space-1);
    overflow-x: auto;
    overflow-y: hidden;
    border-bottom: 1px solid var(--border-default);
    scrollbar-width: none;
  }
  .tab-strip::-webkit-scrollbar {
    display: none;
  }
  button {
    flex: 0 0 auto;
    padding: var(--space-2) var(--space-3);
    border: 0;
    border-bottom: 2px solid transparent;
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-sm);
    white-space: nowrap;
  }
  button:hover {
    color: var(--text-primary);
  }
  button[aria-selected="true"] {
    border-bottom-color: var(--accent-blue);
    color: var(--text-primary);
  }
  button:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
  }
</style>
