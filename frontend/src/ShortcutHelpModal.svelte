<script lang="ts">
  import {
    Button,
    KbdBadge,
    Modal,
    SelectDropdown,
    formatShortcutKeys,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import type { Tag } from "./generated/docbank.js";
  import { TAG_HOTKEYS, type TagHotkey, type TagHotkeyBindings } from "./tag-hotkeys.js";

  interface Props {
    context?: "documents" | "photos";
    vaultReady: boolean;
    catalog: Tag[];
    catalogTotal: number;
    bindings: TagHotkeyBindings;
    onbindingchange: (key: TagHotkey, tagID: string) => void;
    onclose: () => void;
    photoShortcuts?: { combo: string; description?: string | undefined }[];
  }

  let {
    context = "documents",
    vaultReady,
    catalog,
    catalogTotal,
    bindings,
    onbindingchange,
    onclose,
    photoShortcuts = [],
  }: Props = $props();

  const shortcuts = [
    { combo: "/", action: "Focus search" },
    { combo: "j", action: "Next row" },
    { combo: "k", action: "Previous row" },
    { combo: "space", action: "Select current row" },
    { combo: "enter", action: "Open current row" },
    { combo: "escape", action: "Clear selection" },
    { combo: "?", action: "Show keyboard shortcuts" },
  ];

  function optionsFor(key: TagHotkey): SelectDropdownOption[] {
    const value = bindings[key] ?? "";
    const options: SelectDropdownOption[] = [{ value: "", label: "Unassigned" }];
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
    {#if context === "documents"}
      <section aria-labelledby="browsing-shortcuts-heading">
        <h3 id="browsing-shortcuts-heading">Navigation</h3>
        <dl class="shortcut-list">
          {#each shortcuts as shortcut (shortcut.combo)}
            <div>
              <dt>{shortcut.action}</dt>
              <dd><KbdBadge keys={formatShortcutKeys(shortcut.combo)} /></dd>
            </div>
          {/each}
        </dl>
      </section>
    {/if}
    {#if context === "photos" && photoShortcuts.length}
      <section aria-labelledby="photo-shortcuts-heading">
        <h3 id="photo-shortcuts-heading">Photos</h3>
        <dl class="shortcut-list">
          {#each photoShortcuts as shortcut (shortcut.combo)}
            <div>
              <dt>{shortcut.description}</dt>
              <dd><KbdBadge keys={formatShortcutKeys(shortcut.combo)} /></dd>
            </div>
          {/each}
        </dl>
      </section>
    {/if}
    {#if context === "documents"}
      <section aria-labelledby="tag-shortcuts-heading">
        <h3 id="tag-shortcuts-heading">Tag hotkeys</h3>
        {#if !vaultReady}
          <p class="unavailable" role="status">
            Tag hotkeys are available once the vault finishes loading.
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
          <p class="boundary">First {catalog.length} of {catalogTotal} tags.</p>
        {/if}
      </section>
    {/if}
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

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold, 600);
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
