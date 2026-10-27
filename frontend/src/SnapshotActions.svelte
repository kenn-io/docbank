<script module lang="ts">
  export type SnapshotActionScope = "selection" | "query";
  export interface SnapshotActionChoice {
    scope: SnapshotActionScope;
    tagID: string;
    assign: boolean;
  }
</script>

<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import CheckIcon from "@lucide/svelte/icons/check";
  import { Button, Modal, SegmentedControl, Spinner } from "@kenn-io/kit-ui";
  import TagPicker from "./TagPicker.svelte";
  import type { Tag } from "./generated/docbank.js";
  import { randomUUID } from "./crypto.js";
  import type { SelectionTarget } from "./selection.js";
  import { ACTION_MAX_BYTES } from "./actionRecovery.js";
  import type { BatchTagReceipt } from "./batch-tags.js";
  import { TagChangeRun } from "./tagChangeRun.svelte.js";
  import { documentCount, tagChangeConsequence, tagChangeDone } from "./tagChangeText.js";

  interface Props {
    session: string;
    scope: SnapshotActionScope;
    /** Documents the change covers: the selection, or every result. */
    count: number;
    /** Exact selected documents; used only for the selection scope. */
    targets?: readonly SelectionTarget[];
    catalog: readonly Tag[];
    disabled: boolean;
    /** Whole-result preparation is running in the workspace. */
    preparing?: boolean;
    onstart?: (choice: SnapshotActionChoice) => void;
    onchanged?: (receipt: BatchTagReceipt) => void;
    onunknown?: () => void;
    oncreated?: () => void;
    onimport?: (bytes: Uint8Array) => void;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    errorMessage?: string;
  }

  let {
    session,
    scope,
    count,
    targets = [],
    catalog,
    disabled,
    preparing = false,
    onstart,
    onchanged,
    onunknown,
    oncreated,
    onimport,
    onclose,
    onauthfailure,
    errorMessage = "",
  }: Props = $props();
  let tag = $state<Tag>();
  let assign = $state(true);
  let reading = $state(false);
  let readFailure = $state("");
  let receipt = $state<BatchTagReceipt>();
  const run = new TagChangeRun({
    session: untrack(() => session),
    onauthfailure: (cause) => onauthfailure(cause),
    onconfirmed: (confirmed) => {
      receipt = confirmed;
      onchanged?.(confirmed);
    },
    onunknown: () => onunknown?.(),
    unknownMessage: "The result is unknown. Check these documents' tags before trying again.",
    staleMessage: "Some documents changed. Close and select them again.",
  });
  const applying = $derived(run.pending);
  const stale = $derived(run.stale);
  const failure = $derived(readFailure || run.failure);
  let fileInput = $state<HTMLInputElement>();
  // The selection is captured when the dialog opens; later clicks on the page do not change it.
  const captured = untrack(() => targets.map((target) => ({ ...target })));
  let alive = true;
  onDestroy(() => {
    alive = false;
    run.destroy();
  });

  const busy = $derived(disabled || preparing || applying || reading);
  const locked = $derived(busy || stale || receipt !== undefined);
  const consequence = $derived(
    tag
      ? tagChangeConsequence(assign, tag.name, count)
      : `${assign ? "Adds a tag to" : "Removes a tag from"} ${documentCount(count)}.`,
  );

  function apply(): void {
    if (locked || !tag || count === 0) return;
    readFailure = "";
    run.failure = "";
    if (scope === "query") {
      onstart?.({ scope, tagID: tag.id, assign });
      return;
    }
    void run.submit({
      operation_id: randomUUID(),
      tag_id: tag.id,
      assign,
      nodes: captured.map((target) => ({ ...target })),
    });
  }

  async function importFile(event: Event): Promise<void> {
    const input = event.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = "";
    if (!file || busy) return;
    reading = true;
    readFailure = "";
    run.failure = "";
    try {
      if (file.size > ACTION_MAX_BYTES)
        throw new Error("The action recovery file exceeds 128 MiB.");
      const bytes = new Uint8Array(await file.arrayBuffer());
      if (alive) onimport?.(bytes);
    } catch (cause) {
      readFailure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      reading = false;
    }
  }

  function close(): void {
    if (applying) return;
    onclose();
  }
</script>

<Modal
  title={scope === "query" ? "Tag all results" : "Tag selected documents"}
  tone="info"
  width="480px"
  maxWidth="min(480px, calc(100vw - 32px))"
  ariaLabel={scope === "query" ? "Tag all results" : "Tag selected documents"}
  onclose={close}
  closeOnOverlayClick={!busy}
>
  <div class="tag-change">
    <div class="choice">
      <TagPicker
        class="tag-change__picker"
        {session}
        value={tag?.id ?? ""}
        tags={catalog}
        title="Tag"
        placeholder="Tag name"
        disabled={locked}
        reopenAfterPick={false}
        onchange={(id) => (tag = catalog.find((item) => item.id === id))}
        onpick={(picked, created) => {
          tag = picked;
          if (created) oncreated?.();
        }}
        {onauthfailure}
      />
      <SegmentedControl
        ariaLabel="Tag change"
        options={[
          { value: "add", label: "Add" },
          { value: "remove", label: "Remove" },
        ]}
        value={assign ? "add" : "remove"}
        onchange={(value) => (assign = value === "add")}
        disabled={locked}
      />
    </div>

    {#if receipt}
      <p class="status done" role="status">
        <CheckIcon size={14} aria-hidden="true" />
        {tagChangeDone(
          receipt.assign,
          tag?.name ?? "",
          receipt.nodes.filter((node) => node.changed).length,
          receipt.nodes.length,
        )}
      </p>
    {:else}
      <p class="consequence" class:pending={!tag}>{consequence}</p>
    {/if}
    {#if applying || preparing}
      <p class="status" role="status">
        <Spinner size={14} />
        {preparing ? `Preparing ${documentCount(count)}…` : "Applying…"}
      </p>
    {/if}
    {#if failure || errorMessage}<p class="alert" role="alert">{failure || errorMessage}</p>{/if}
  </div>
  {#snippet footer()}
    {#if scope === "query" && !receipt}
      <span class="footer-start">
        <Button size="sm" surface="soft" disabled={busy} onclick={() => fileInput?.click()}
          >{reading ? "Reading…" : "Resume from file…"}</Button
        >
        <input
          class="file"
          type="file"
          accept="application/json,.json"
          aria-label="Resume from a recovery file"
          tabindex="-1"
          bind:this={fileInput}
          disabled={busy}
          onchange={(event) => void importFile(event)}
        />
      </span>
    {/if}
    {#if receipt}
      <Button tone="info" surface="solid" onclick={onclose}>Done</Button>
    {:else}
      <Button surface="soft" disabled={applying} onclick={close}>Cancel</Button>
      <Button tone="info" surface="solid" disabled={locked || !tag || count === 0} onclick={apply}
        >{scope === "query" ? "Continue" : "Apply"}</Button
      >
    {/if}
  {/snippet}
</Modal>

<style>
  .tag-change {
    display: grid;
    gap: var(--space-4);
  }
  .tag-change p {
    margin: 0;
  }
  .choice {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .choice :global(.tag-change__picker) {
    flex: 1;
    min-width: 0;
  }
  .consequence {
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold, 600);
  }
  .consequence.pending {
    color: var(--text-muted);
    font-weight: inherit;
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .status.done {
    color: var(--accent-green);
  }
  .alert {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
  .footer-start {
    margin-inline-end: auto;
  }
  .file {
    position: absolute;
    width: 1px;
    height: 1px;
    opacity: 0;
    pointer-events: none;
  }
</style>
