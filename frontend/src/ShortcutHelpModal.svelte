<script lang="ts">
  import {
    Button,
    KbdBadge,
    Modal,
    SelectDropdown,
    formatShortcutKeys,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import type { Tag } from "./api.js";
  import {
    TAG_HOTKEYS,
    type TagHotkey,
    type TagHotkeyBindings,
  } from "./tag-hotkeys.js";

  interface Props {
    vaultReady: boolean;
    catalog: Tag[];
    catalogTotal: number;
    bindings: TagHotkeyBindings;
    onbindingchange: (key: TagHotkey, tagID: string) => void;
    onclose: () => void;
  }

  let {
    vaultReady,
    catalog,
    catalogTotal,
    bindings,
    onbindingchange,
    onclose,
  }: Props = $props();

  const shortcuts = [
    { combo: "/", action: "Focus search" },
    { combo: "j", action: "Next loaded row" },
    { combo: "k", action: "Previous loaded row" },
    { combo: "space", action: "Toggle inspected file selection" },
    { combo: "enter", action: "Open inspected row" },
    { combo: "escape", action: "Clear page selection" },
    { combo: "?", action: "Show keyboard shortcuts" },
  ];

  function optionsFor(key: TagHotkey): SelectDropdownOption[] {
    const value = bindings[key] ?? "";
    const options: SelectDropdownOption[] = [
      { value: "", label: "Unassigned" },
    ];
    if (value && !catalog.some((tag) => tag.id === value)) {
      options.push({
        value,
        label:
          catalogTotal > catalog.length
            ? `Not in loaded catalog · ${value}`
            : `Unavailable or deleted tag · ${value}`,
      });
    }
    options.push(
      ...catalog.map((tag) => ({
        value: tag.id,
        label: tag.name,
      })),
    );
    return options;
  }
</script>

<Modal
  title="Keyboard shortcuts"
  tone="info"
  width="680px"
  maxWidth="min(680px, calc(100vw - 32px))"
  ariaLabel="Keyboard shortcuts"
  {onclose}
>
  <div class="shortcut-help">
    <section aria-labelledby="browsing-shortcuts-heading">
      <div class="section-heading">
        <span>BROWSING</span>
        <strong id="browsing-shortcuts-heading">Loaded-page shortcuts</strong>
      </div>
      <dl class="shortcut-list">
        {#each shortcuts as shortcut (shortcut.combo)}
          <div>
            <dt>{shortcut.action}</dt>
            <dd><KbdBadge keys={formatShortcutKeys(shortcut.combo)} /></dd>
          </div>
        {/each}
      </dl>
      <p class="boundary">
        Row navigation stops at the first and last item currently loaded. It
        does not wrap or fetch another page.
      </p>
    </section>

    <section aria-labelledby="tag-shortcuts-heading">
      <div class="section-heading">
        <span>TAG HOTKEYS</span>
        <strong id="tag-shortcuts-heading">Assign digits to vault tags</strong>
      </div>
      {#if !vaultReady}
        <p class="unavailable" role="status">
          Tag shortcuts are unavailable until this browser session confirms the vault identity.
        </p>
      {/if}
      <div class="tag-bindings">
        {#each TAG_HOTKEYS as key (key)}
          <div class="tag-binding">
            <KbdBadge keys={formatShortcutKeys(key)} ariaLabel={`Digit ${key}`} />
            <SelectDropdown
              value={bindings[key] ?? ""}
              options={optionsFor(key)}
              title={`Tag shortcut ${key}`}
              disabled={!vaultReady}
              onchange={(tagID) => onbindingchange(key, tagID)}
            />
          </div>
        {/each}
      </div>
      {#if catalogTotal > catalog.length}
        <p class="boundary">
          Showing the first {catalog.length} of {catalogTotal} tag definitions.
          Saved IDs outside this loaded catalog are retained as unavailable.
        </p>
      {/if}
    </section>
  </div>
  {#snippet footer()}
    <Button surface="soft" onclick={onclose}>Done</Button>
  {/snippet}
</Modal>

<style>
  .shortcut-help {
    display: grid;
    gap: var(--space-6);
  }

  section {
    display: grid;
    gap: var(--space-3);
  }

  .section-heading {
    display: grid;
    gap: var(--space-1);
  }

  .section-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold, 600);
    letter-spacing: var(--letter-spacing-label, 0.08em);
  }

  .section-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .shortcut-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 1px;
    margin: 0;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--border-muted);
  }

  .shortcut-list > div {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-3);
    background: var(--bg-inset);
  }

  .shortcut-list dt {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .shortcut-list dd {
    margin: 0;
  }

  .tag-bindings {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .tag-binding {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    align-items: center;
    gap: var(--space-2);
  }

  .tag-binding :global(.kit-select-dropdown) {
    min-width: 0;
    width: 100%;
  }

  .boundary,
  .unavailable {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.5;
  }

  .unavailable {
    padding: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent-amber) 32%, transparent);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-amber) 8%, transparent);
    color: var(--text-secondary);
  }

  @media (max-width: 640px) {
    .shortcut-list,
    .tag-bindings {
      grid-template-columns: 1fr;
    }
  }
</style>
