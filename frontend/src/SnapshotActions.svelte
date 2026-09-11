<script module lang="ts">
  export type SnapshotActionScope = "selection" | "query";
  export interface SnapshotActionChoice {
    scope: SnapshotActionScope;
    tagID: string;
    assign: boolean;
  }
</script>

<script lang="ts">
  import { Button, Modal, SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui";
  import type { Tag } from "./api.js";

  interface Props {
    selectedCount: number;
    total: number;
    catalog: readonly Tag[];
    catalogTotal: number;
    disabled: boolean;
    onstart: (choice: SnapshotActionChoice) => void;
    onimport: (bytes: Uint8Array) => void;
    onresume: () => void;
    onclose: () => void;
    errorMessage?: string;
  }

  let {
    selectedCount,
    total,
    catalog,
    catalogTotal,
    disabled,
    onstart,
    onimport,
    onresume,
    onclose,
    errorMessage = "",
  }: Props = $props();
  let tagID = $state("");
  let reading = $state(false);
  let failure = $state("");

  const options = $derived<SelectDropdownOption[]>([
    { value: "", label: "Choose a tag…" },
    ...catalog.map((tag) => ({ value: tag.id, label: tag.name })),
  ]);

  function start(scope: SnapshotActionScope, assign: boolean): void {
    if (disabled || reading || !tagID || (scope === "selection" && selectedCount === 0)) return;
    onstart({ scope, tagID, assign });
  }

  async function importFile(event: Event): Promise<void> {
    const input = event.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = "";
    if (!file || disabled || reading) return;
    reading = true;
    failure = "";
    try {
      onimport(new Uint8Array(await file.arrayBuffer()));
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      reading = false;
    }
  }
</script>

<Modal title="Tag frozen snapshot" tone="info" width="680px"
  maxWidth="min(680px, calc(100vw - 32px))" ariaLabel="Tag frozen snapshot"
  onclose={onclose} closeOnOverlayClick={!disabled && !reading}>
  <div class="snapshot-actions">
    <p class="warning">
      Recovery files contain private document identities, hashes, sizes, and revisions. Store them with the same care as vault metadata.
    </p>

    <SelectDropdown value={tagID} {options} title="Tag for snapshot action"
      disabled={disabled || reading} onchange={(value) => (tagID = value)} />
    {#if catalogTotal > catalog.length}
      <p class="hint">Showing {catalog.length} of {catalogTotal} tag definitions.</p>
    {/if}

    <section aria-labelledby="visible-action-heading">
      <h3 id="visible-action-heading">Visible selection</h3>
      <p>{selectedCount} document{selectedCount === 1 ? "" : "s"} selected on this visible page. This does not include unselected or other snapshot pages.</p>
      <div class="actions">
        <Button tone="info" disabled={disabled || reading || !tagID || selectedCount === 0}
          onclick={() => start("selection", true)}>Add tag to visible selection</Button>
        <Button disabled={disabled || reading || !tagID || selectedCount === 0}
          onclick={() => start("selection", false)}>Remove tag from visible selection</Button>
      </div>
    </section>

    <section aria-labelledby="whole-action-heading">
      <h3 id="whole-action-heading">Whole frozen query</h3>
      <p>Prepare a recoverable action for all {total} documents in the frozen query, including pages not currently visible.</p>
      <div class="actions">
        <Button tone="info" disabled={disabled || reading || !tagID || total === 0}
          onclick={() => start("query", true)}>Add tag to whole query</Button>
        <Button disabled={disabled || reading || !tagID || total === 0}
          onclick={() => start("query", false)}>Remove tag from whole query</Button>
      </div>
    </section>

    <section aria-labelledby="recover-action-heading">
      <h3 id="recover-action-heading">Recover an action</h3>
      <p>Selecting a recovery file only validates and stages it. It never runs a mutation by itself.</p>
      <Button disabled={disabled || reading} onclick={onresume}>Resume retained action</Button>
      <label class:disabled={disabled || reading}>
        <span>Import action recovery file</span>
        <input type="file" accept="application/json,.json" disabled={disabled || reading}
          onchange={(event) => void importFile(event)} />
      </label>
    </section>

    {#if failure || errorMessage}<p role="alert">{failure || errorMessage}</p>{/if}
  </div>
  {#snippet footer()}<Button surface="soft" disabled={reading} onclick={onclose}>Cancel</Button>{/snippet}
</Modal>

<style>
  .snapshot-actions { display: grid; gap: var(--space-5); }
  .snapshot-actions p, .snapshot-actions h3 { margin: 0; }
  .snapshot-actions section { display: grid; gap: var(--space-3); padding-block-start: var(--space-4); border-top: 1px solid var(--border-default); }
  .snapshot-actions h3 { font-size: var(--font-size-sm); }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-3); }
  .warning { padding: var(--space-3); border: 1px solid var(--border-warning); border-radius: var(--radius-md); background: var(--bg-warning-soft); }
  .hint { color: var(--text-muted); font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--space-2); font-size: var(--font-size-sm); font-weight: 600; }
  label.disabled { opacity: 0.6; }
</style>
