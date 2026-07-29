<script lang="ts">
  import RotateCcwIcon from "@lucide/svelte/icons/rotate-ccw";
  import { Button, Modal, Spinner } from "@kenn-io/kit-ui";
  import { APIError, restoreNode, type Node } from "./api.js";
  import { formatDate } from "./format.js";

  let {
    session,
    node,
    onclose,
    onrestored,
    onauthfailure,
  }: {
    session: string;
    node: Node;
    onclose: () => void;
    onrestored: (receipt: Node) => void;
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
      const receipt = await restoreNode(session, node.id, node.revision);
      onrestored(receipt);
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
  title="Restore from trash?"
  tone="info"
  ariaLabel={`Restore ${node.name} from trash`}
  onclose={close}
  closeOnOverlayClick={!pending}
>
  <div class="restore-confirmation">
    <div class="target">
      <RotateCcwIcon size="20" aria-hidden="true" />
      <div>
        <strong>{node.name}</strong>
        <span>
          Stable node id:{node.id} · revision {node.revision}
        </span>
        <span>Trashed {formatDate(node.trashed_at ?? "")}</span>
      </div>
    </div>
    <p>
      Docbank will return this {node.kind === "dir" ? "folder" : "document"} to
      its original live parent when that directory still exists. Otherwise it
      returns beneath <code>/</code>; a name collision receives a suffix.
    </p>
    <p class="boundary">
      Restore keeps the same stable node and retained content. It does not roll
      back versions or alter permanent audited history.
    </p>
    {#if failure}
      <p class="failure" role="alert">{failure}</p>
    {/if}
  </div>
  {#snippet footer()}
    <Button surface="soft" disabled={pending} onclick={close}>Leave in trash</Button>
    <Button
      tone="info"
      surface="solid"
      disabled={pending}
      onclick={() => void confirm()}
    >
      {#if pending}
        <Spinner size={14} /> Restoring…
      {:else}
        <RotateCcwIcon size="14" /> Restore
      {/if}
    </Button>
  {/snippet}
</Modal>

<style>
  .restore-confirmation {
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
    color: var(--accent-blue);
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
