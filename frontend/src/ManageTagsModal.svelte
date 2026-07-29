<script lang="ts">
  import { untrack } from "svelte";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import TagIcon from "@lucide/svelte/icons/tag";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Chip,
    IconButton,
    Modal,
    SelectDropdown,
    Spinner,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    changeNodeTag,
    type Node,
    type Tag,
    type TagAssignmentReceipt,
  } from "./api.js";

  interface Props {
    session: string;
    node: Node;
    catalog: Tag[];
    catalogTotal: number;
    assignedTags: Tag[];
    assignedTotal: number;
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
    onclose,
    onchanged,
    onauthfailure,
  }: Props = $props();

  let currentNode = $state(untrack(() => node));
  let assigned = $state(untrack(() => [...assignedTags]));
  let currentAssignedTotal = $state(untrack(() => assignedTotal));
  let selectedTagID = $state("");
  let pendingTagID = $state("");
  let failure = $state("");
  let notice = $state("");

  const available = $derived(
    catalog.filter((tag) => !assigned.some((item) => item.id === tag.id)),
  );
  const options = $derived<SelectDropdownOption[]>([
    { value: "", label: available.length ? "Choose a tag…" : "No available tags" },
    ...available.map((tag) => ({
      value: tag.id,
      label: `${tag.name} (${tag.assignment_count})`,
      triggerLabel: tag.name,
    })),
  ]);

  function close(): void {
    if (!pendingTagID) onclose();
  }

  async function change(tag: Tag, assign: boolean): Promise<void> {
    if (pendingTagID) return;
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
        assigned = [
          ...assigned.filter((item) => item.id !== tag.id),
          receipt.tag,
        ].sort((left, right) => left.name.localeCompare(right.name));
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
    <header class="target">
      <TagIcon size="19" aria-hidden="true" />
      <div>
        <strong>{node.name || "/"}</strong>
        <span>Stable node id:{currentNode.id} · revision {currentNode.revision}</span>
      </div>
    </header>

    <section aria-labelledby="assigned-tags-heading">
      <div class="section-heading">
        <div>
          <span>ASSIGNED TAGS</span>
          <strong id="assigned-tags-heading">{assigned.length} visible</strong>
        </div>
        {#if currentAssignedTotal > assigned.length}
          <small>First {assigned.length} of {currentAssignedTotal}</small>
        {/if}
      </div>
      {#if assigned.length === 0}
        <p class="empty">No tags are assigned to this node.</p>
      {:else}
        <div class="assigned-list">
          {#each assigned as tag (tag.id)}
            <div class="assigned-tag">
              <div>
                <Chip size="sm" tone="workspace" uppercase={false}>{tag.name}</Chip>
                <small>{tag.assignment_count} total assignment{tag.assignment_count === 1 ? "" : "s"}</small>
              </div>
              <IconButton
                size="sm"
                ariaLabel={`Remove tag ${tag.name}`}
                title={`Remove ${tag.name}`}
                disabled={pendingTagID !== ""}
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
        </div>
      {/if}
    </section>

    <section aria-labelledby="add-tag-heading">
      <div class="section-heading">
        <div>
          <span>ADD AN EXISTING TAG</span>
          <strong id="add-tag-heading">Choose from the vault catalog</strong>
        </div>
      </div>
      <div class="add-row">
        <SelectDropdown
          value={selectedTagID}
          {options}
          title="Tag to assign"
          disabled={pendingTagID !== "" || available.length === 0}
          onchange={(value) => (selectedTagID = value)}
        />
        <Button
          size="sm"
          tone="info"
          surface="solid"
          disabled={!selectedTagID || pendingTagID !== ""}
          onclick={addSelected}
        >
          {#if pendingTagID === selectedTagID}
            <Spinner size={13} />
          {:else}
            <PlusIcon size="14" aria-hidden="true" />
          {/if}
          Add tag
        </Button>
      </div>
      {#if catalogTotal > catalog.length}
        <p class="bounded">
          Showing the first {catalog.length} of {catalogTotal} tag definitions.
          Use the CLI or API for definitions outside this catalog page.
        </p>
      {/if}
    </section>

    {#if notice}<p class="notice" role="status">{notice}</p>{/if}
    {#if failure}<p class="failure" role="alert">{failure}</p>{/if}

    <p class="boundary">
      This changes only the selected node’s tag assignments. Creating,
      renaming, deleting, or bulk-applying tag definitions remains a CLI or
      authenticated API workflow.
    </p>
  </div>
  {#snippet footer()}
    <Button surface="soft" disabled={pendingTagID !== ""} onclick={close}>Done</Button>
  {/snippet}
</Modal>

<style>
  .manage-tags {
    display: grid;
    gap: var(--space-5);
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
    color: var(--accent-teal);
  }

  .target > div {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }

  .target strong {
    overflow-wrap: anywhere;
    color: var(--text-primary);
  }

  .target span,
  small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  section {
    display: grid;
    gap: var(--space-3);
  }

  .section-heading {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .section-heading > div {
    display: grid;
    gap: 1px;
  }

  .section-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .section-heading strong {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .assigned-list {
    display: grid;
    gap: var(--space-2);
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
  .boundary,
  .notice,
  .failure {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .empty,
  .bounded,
  .boundary {
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

  .boundary {
    padding-top: var(--space-4);
    border-top: 1px solid var(--border-subtle);
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
