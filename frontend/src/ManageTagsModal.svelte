<script lang="ts">
  import { untrack } from "svelte";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    IconButton,
    Modal,
    Spinner,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    changeNodeTag,
    type Node,
    type Tag,
    type TagAssignmentReceipt,
  } from "./api.js";
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
  let selectedTagID = $state("");
  let pendingTagID = $state("");
  let failure = $state("");
  let notice = $state("");

  const available = $derived(
    catalog.filter((tag) => !assigned.some((item) => item.id === tag.id)),
  );
  const assignedGroups = $derived(groupTags(assigned));

  function close(): void {
    if (!pendingTagID) onclose();
  }

  async function change(tag: Tag, assign: boolean): Promise<void> {
    if (disabled || pendingTagID) return;
    pendingTagID = tag.id;
    failure = "";
    notice = "";
    try {
      const receipt = await changeNodeTag(
        session,
        currentNode.id,
        currentNode.revision,
        tag.id,
        assign,
      );
      currentNode = receipt.node;
      if (assign) {
        assigned = sortTags([
          ...assigned.filter((item) => item.id !== tag.id),
          receipt.tag,
        ]);
        if (receipt.changed) currentAssignedTotal += 1;
        notice = receipt.changed
          ? `Added ${receipt.tag.name}.`
          : `${receipt.tag.name} was already assigned.`;
      } else {
        assigned = assigned.filter((item) => item.id !== tag.id);
        if (receipt.changed) {
          currentAssignedTotal = Math.max(0, currentAssignedTotal - 1);
        }
        notice = receipt.changed
          ? `Removed ${receipt.tag.name}.`
          : `${receipt.tag.name} was already absent.`;
      }
      selectedTagID = "";
      onchanged(receipt, assign);
    } catch (cause) {
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        return;
      }
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      pendingTagID = "";
    }
  }

  function addSelected(): void {
    const tag = available.find((item) => item.id === selectedTagID);
    if (tag) void change(tag, true);
  }
</script>

<Modal
  title="Manage tags"
  tone="info"
  width="620px"
  maxWidth="min(620px, calc(100vw - 32px))"
  ariaLabel={`Manage tags for ${node.name || "/"}`}
  onclose={close}
  closeOnOverlayClick={!pendingTagID}
>
  <div class="manage-tags">
    <p class="target">{node.name || "/"}</p>

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
                <small>{tag.assignment_count} total assignment{tag.assignment_count === 1 ? "" : "s"}</small>
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
      <p class="bounded">Showing the first {assigned.length} of {currentAssignedTotal} assigned tags.</p>
    {/if}

    <div class="add-row">
      <TagPicker
        value={selectedTagID}
        tags={available}
        title="Tag to assign"
        placeholder={available.length ? "Choose a tag…" : "No available tags"}
        disabled={disabled || pendingTagID !== "" || available.length === 0}
        onchange={(value) => (selectedTagID = value)}
      />
      <Button
        size="sm"
        tone="info"
        surface="solid"
        disabled={disabled || !selectedTagID || pendingTagID !== ""}
        onclick={addSelected}
      >
        {#if pendingTagID !== "" && pendingTagID === selectedTagID}
          <Spinner size={13} />
        {:else}
          <PlusIcon size="14" aria-hidden="true" />
        {/if}
        Add tag
      </Button>
    </div>
    {#if catalogTotal > catalog.length}
      <p class="bounded">Showing the first {catalog.length} of {catalogTotal} tags.</p>
    {/if}

    {#if notice}<p class="notice" role="status">{notice}</p>{/if}
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

  .target {
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold, 600);
  }

  small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .assigned-list {
    display: grid;
    gap: var(--space-2);
  }

  .assigned-group-heading {
    padding: var(--space-1) var(--space-2) 0;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: 0.05em;
    overflow-wrap: anywhere;
    text-transform: uppercase;
  }

  .assigned-tag {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-subtle);
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
  .notice,
  .failure {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .empty,
  .bounded {
    color: var(--text-muted);
  }

  .notice,
  .failure {
    padding: var(--space-3);
    border-radius: var(--radius-md);
  }

  .notice {
    border: 1px solid color-mix(in srgb, var(--accent-green) 30%, transparent);
    background: color-mix(in srgb, var(--accent-green) 8%, transparent);
    color: var(--accent-green);
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
