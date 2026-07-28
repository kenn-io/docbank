<script lang="ts">
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import { Button, Modal, Spinner } from "@kenn-io/kit-ui";
  import { APIError, trashNode, type Node } from "./api.js";

  let {
    session,
    node,
    path,
    onclose,
    ontrashed,
    onauthfailure,
  }: {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    ontrashed: (receipt: Node) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let pending = $state(false);
  let failure = $state("");

  function close(): void {
    if (!pending) onclose();
  }

  async function confirm(): Promise<void> {
    if (pending) return;
    pending = true;
    failure = "";
    try {
      const receipt = await trashNode(session, node.id, node.revision);
      ontrashed(receipt);
    } catch (cause) {
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        return;
      }
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      pending = false;
    }
  }
</script>

<Modal
  title="Move to trash?"
  tone="danger"
  ariaLabel={`Move ${node.name} to trash`}
  onclose={close}
  closeOnOverlayClick={!pending}
>
  <div class="trash-confirmation">
    <div class="target">
      <Trash2Icon size="20" aria-hidden="true" />
      <div>
        <strong>{node.name}</strong>
        <code>{path}</code>
        <span>Stable node id:{node.id} · revision {node.revision}</span>
      </div>
    </div>
    <p>
      {#if node.kind === "dir"}
        This folder and every live item inside it will leave the current tree together.
      {:else}
        This document will leave the current tree.
      {/if}
      It remains recoverable from trash.
    </p>
    <p class="boundary">
      This does not empty trash, reclaim stored bytes, or erase permanent audited history.
    </p>
    {#if failure}
      <p class="failure" role="alert">{failure}</p>
    {/if}
  </div>
  {#snippet footer()}
    <Button surface="soft" disabled={pending} onclick={close}>Keep in Docbank</Button>
    <Button
      tone="danger"
      surface="solid"
      disabled={pending}
      onclick={() => void confirm()}
    >
      {#if pending}<Spinner size={14} /> Moving…{:else}<Trash2Icon size="14" /> Move to trash{/if}
    </Button>
  {/snippet}
</Modal>

<style>
  .trash-confirmation {
    display: grid;
    gap: var(--space-4);
  }

  .target {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    align-items: start;
    gap: var(--space-3);
    padding: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .target > :global(svg) {
    margin-top: 2px;
    color: var(--accent-red);
  }

  .target > div {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }

  .target strong {
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .target code {
    overflow-wrap: anywhere;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .target span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  p {
    margin: 0;
    color: var(--text-secondary);
    line-height: 1.5;
  }

  .boundary {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .failure {
    padding: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent-red) 35%, transparent);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
</style>
