<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, IconButton, Modal, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { changeNodeTag } from "./receipts.js";
  import { type Node, type Tag, type TagAssignmentReceipt } from "./generated/docbank.js";
  import TagLabel from "./TagLabel.svelte";
  import TagPicker from "./TagPicker.svelte";
  import { groupTags, sortTags } from "./tagPresentation.js";

  interface Props {
    session: string;
    node: Node;
    catalog: Tag[];
    catalogTotal: number;
    assignedTags: Tag[];
    assignedTotal: number;
    disabled: boolean;
    onclose: () => void;
    onchanged: (receipt: TagAssignmentReceipt, assigned: boolean) => void;
    onauthfailure: (cause: unknown) => void;
  }

  let {
    session,
    node,
    catalog,
    catalogTotal,
    assignedTags,
    assignedTotal,
    disabled,
    onclose,
    onchanged,
    onauthfailure,
  }: Props = $props();

  let currentNode = $state(untrack(() => node));
  let assigned = $state(untrack(() => sortTags(assignedTags)));
  let currentAssignedTotal = $state(untrack(() => assignedTotal));
  let pendingTagID = $state("");
  let failure = $state("");
  let destroyed = false;
  let root = $state<HTMLElement>();
  // The modal's own focus handling runs first; the tag field takes focus after it.
  onMount(() => {
    void tick().then(() =>
      requestAnimationFrame(() => root?.querySelector<HTMLInputElement>(".add-row input")?.focus()),
    );
  });
  onDestroy(() => {
    destroyed = true;
  });

  const assignedGroups = $derived(groupTags(assigned));

  function close(): void {
    if (!pendingTagID) onclose();
  }

  async function change(tag: Tag, assign: boolean): Promise<void> {
    if (disabled || pendingTagID) return;
    const requestSession = session;
    pendingTagID = tag.id;
    failure = "";
    try {
      const receipt = await changeNodeTag(
        requestSession,
        currentNode.id,
        currentNode.revision,
        tag.id,
        assign,
      );
      if (destroyed || session !== requestSession) return;
      currentNode = receipt.node;
      if (assign) {
        assigned = sortTags([...assigned.filter((item) => item.id !== tag.id), receipt.tag]);
        if (receipt.changed) currentAssignedTotal += 1;
      } else {
        assigned = assigned.filter((item) => item.id !== tag.id);
        if (receipt.changed) {
          currentAssignedTotal = Math.max(0, currentAssignedTotal - 1);
        }
      }
      onchanged(receipt, assign);
    } catch (cause) {
      if (destroyed || session !== requestSession) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        return;
      }
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!destroyed) pendingTagID = "";
    }
  }
</script>

<Modal
  title={`Tags for ${node.name || "/"}`}
  tone="info"
  width="620px"
  maxWidth="min(620px, calc(100vw - 32px))"
  ariaLabel={`Manage tags for ${node.name || "/"}`}
  onclose={close}
  closeOnOverlayClick={!pendingTagID}
>
  <div class="manage-tags" bind:this={root}>
    {#if assigned.length === 0}
      <p class="empty">No tags assigned.</p>
    {:else}
      <div class="assigned-list" role="group" aria-label="Assigned tags">
        {#each assignedGroups as group}
          {#if group.name}
            <div class="assigned-group-heading">{group.name}</div>
          {/if}
          {#each group.tags as item (item.tag.id)}
            {@const tag = item.tag}
            <div class="assigned-tag">
              <div>
                <TagLabel {tag} size="sm" />
                <small
                  >{tag.assignment_count.toLocaleString()} file{tag.assignment_count === 1
                    ? ""
                    : "s"}</small
                >
              </div>
              <IconButton
                size="sm"
                ariaLabel={`Remove tag ${tag.name}`}
                title={`Remove ${tag.name}`}
                disabled={disabled || pendingTagID !== ""}
                onclick={() => void change(tag, false)}
              >
                {#if pendingTagID === tag.id}
                  <Spinner size={13} />
                {:else}
                  <XIcon size="14" aria-hidden="true" />
                {/if}
              </IconButton>
            </div>
          {/each}
        {/each}
      </div>
    {/if}
    {#if currentAssignedTotal > assigned.length}
      <p class="bounded">
        Showing the first {assigned.length} of {currentAssignedTotal} assigned tags.
      </p>
    {/if}

    <div class="add-row">
      <TagPicker
        {session}
        value=""
        tags={[
          ...catalog,
          ...assigned.filter((tag) => !catalog.some((item) => item.id === tag.id)),
        ]}
        excludedTagIDs={assigned.map((tag) => tag.id)}
        title="Tag to assign"
        placeholder="Tag name"
        disabled={disabled || pendingTagID !== ""}
        onchange={() => undefined}
        onpick={(tag) => change(tag, true)}
        {onauthfailure}
      />
    </div>
    {#if catalogTotal > catalog.length}
      <p class="bounded">Showing the first {catalog.length} of {catalogTotal} tags.</p>
    {/if}

    {#if failure}<p class="failure" role="alert">{failure}</p>{/if}
  </div>
  {#snippet footer()}
    <Button surface="soft" disabled={pendingTagID !== ""} onclick={close}>Done</Button>
  {/snippet}
</Modal>

<style>
  .manage-tags {
    display: grid;
    gap: var(--space-4);
  }

  small {
    color: var(--text-muted);
    font-size: var(--font-size-md);
  }

  .assigned-list {
    display: grid;
    gap: var(--space-2);
  }

  .assigned-group-heading {
    padding: var(--space-1) var(--space-2) 0;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-medium);
    overflow-wrap: anywhere;
  }

  .assigned-tag {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--bg-inset) 75%, transparent);
  }

  .assigned-tag > div {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .add-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-3);
  }

  .empty,
  .bounded,
  .failure {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .empty,
  .bounded {
    color: var(--text-muted);
  }

  .failure {
    padding: var(--space-3);
    border-radius: var(--radius-md);
  }

  .failure {
    border: 1px solid color-mix(in srgb, var(--accent-red) 35%, transparent);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
    color: var(--accent-red);
  }

  @media (max-width: 640px) {
    .add-row {
      grid-template-columns: 1fr;
    }

    .assigned-tag > div {
      align-items: flex-start;
      flex-direction: column;
      gap: var(--space-1);
    }
  }
</style>
