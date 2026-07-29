<script module lang="ts">
  export type TagDefinitionChange =
    | { kind: "created"; tag: import("./api.js").Tag }
    | { kind: "renamed"; tag: import("./api.js").Tag }
    | {
        kind: "deleted";
        tag: import("./api.js").Tag;
        removedAssignments: number;
      };
</script>

<script lang="ts">
  import { untrack } from "svelte";
  import CheckIcon from "@lucide/svelte/icons/check";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import TagIcon from "@lucide/svelte/icons/tag";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Chip,
    IconButton,
    Modal,
    Spinner,
    TextInput,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    createTag,
    deleteTag,
    renameTag,
    type Tag,
  } from "./api.js";

  let {
    session,
    catalog,
    catalogTotal,
    disabled,
    onclose,
    onchanged,
    onauthfailure,
  }: {
    session: string;
    catalog: Tag[];
    catalogTotal: number;
    disabled: boolean;
    onclose: () => void;
    onchanged: (change: TagDefinitionChange) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let current = $state(untrack(() => [...catalog]));
  let currentTotal = $state(untrack(() => catalogTotal));
  let newName = $state("");
  let editingID = $state("");
  let editName = $state("");
  let deleting = $state<Tag | null>(null);
  let pending = $state(false);
  let failure = $state("");
  let notice = $state("");

  function close(): void {
    if (!pending) onclose();
  }

  function reportFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      return;
    }
    failure = cause instanceof Error ? cause.message : String(cause);
  }

  function beginRename(tag: Tag): void {
    editingID = tag.id;
    editName = tag.name;
    deleting = null;
    failure = "";
    notice = "";
  }

  function cancelRename(): void {
    editingID = "";
    editName = "";
  }

  async function create(): Promise<void> {
    if (disabled || pending || newName === "") return;
    pending = true;
    failure = "";
    notice = "";
    try {
      const tag = await createTag(session, newName);
      current = [...current, tag].sort((left, right) =>
        left.name.localeCompare(right.name),
      );
      currentTotal += 1;
      newName = "";
      notice = `Created ${tag.name}.`;
      onchanged({ kind: "created", tag });
    } catch (cause) {
      reportFailure(cause);
    } finally {
      pending = false;
    }
  }

  async function saveRename(): Promise<void> {
    const tag = current.find((item) => item.id === editingID);
    if (!tag || disabled || pending || editName === "") return;
    pending = true;
    failure = "";
    notice = "";
    try {
      const renamed = await renameTag(session, tag.id, tag.revision, editName);
      current = current
        .map((item) => (item.id === renamed.id ? renamed : item))
        .sort((left, right) => left.name.localeCompare(right.name));
      editingID = "";
      editName = "";
      notice = `Renamed tag to ${renamed.name}.`;
      onchanged({ kind: "renamed", tag: renamed });
    } catch (cause) {
      reportFailure(cause);
    } finally {
      pending = false;
    }
  }

  async function confirmDelete(): Promise<void> {
    if (!deleting || disabled || pending) return;
    const target = deleting;
    pending = true;
    failure = "";
    notice = "";
    try {
      const receipt = await deleteTag(session, target.id, target.revision);
      current = current.filter((tag) => tag.id !== target.id);
      currentTotal = Math.max(0, currentTotal - 1);
      deleting = null;
      notice = `Deleted ${receipt.tag.name} and removed ${receipt.removed_assignments} assignment${receipt.removed_assignments === 1 ? "" : "s"}.`;
      onchanged({
        kind: "deleted",
        tag: receipt.tag,
        removedAssignments: receipt.removed_assignments,
      });
    } catch (cause) {
      reportFailure(cause);
    } finally {
      pending = false;
    }
  }
</script>

<Modal
  title={deleting ? "Delete tag definition?" : "Tag catalog"}
  tone={deleting ? "danger" : "info"}
  width="680px"
  maxWidth="min(680px, calc(100vw - 32px))"
  ariaLabel={deleting ? `Delete tag ${deleting.name}` : "Manage tag definitions"}
  onclose={close}
  closeOnOverlayClick={!pending}
>
  {#if deleting}
    <div class="delete-confirmation">
      <div class="delete-target">
        <Trash2Icon size="20" aria-hidden="true" />
        <div>
          <strong>{deleting.name}</strong>
          <code>{deleting.id}</code>
          <span>
            Revision {deleting.revision} · {deleting.assignment_count} assignment{deleting.assignment_count === 1 ? "" : "s"}
          </span>
        </div>
      </div>
      <p>
        This permanently removes the tag definition and every assignment that
        currently uses it. Documents and their stored content are not deleted.
      </p>
      <p class="boundary">
        This action cannot be undone. You may create a tag with the same name
        later, but it will have a different stable identity.
      </p>
      {#if failure}<p class="failure" role="alert">{failure}</p>{/if}
    </div>
  {:else}
    <div class="tag-catalog">
      <header class="catalog-heading">
        <TagIcon size="20" aria-hidden="true" />
        <div>
          <strong>Organize the vault’s shared vocabulary</strong>
          <span>
            Names can change; stable tag IDs continue to identify the same definition.
          </span>
        </div>
      </header>

      <section aria-labelledby="create-tag-heading">
        <div class="section-heading">
          <div>
            <span>NEW DEFINITION</span>
            <strong id="create-tag-heading">Create a tag</strong>
          </div>
        </div>
        <form
          class="create-row"
          onsubmit={(event) => {
            event.preventDefault();
            void create();
          }}
        >
          <TextInput
            bind:value={newName}
            block
            ariaLabel="New tag name"
            placeholder="e.g. reviewed"
            disabled={disabled || pending}
          />
          <Button
            size="sm"
            tone="info"
            surface="solid"
            disabled={disabled || pending || newName === ""}
            onclick={() => void create()}
          >
            {#if pending && !editingID}<Spinner size={13} />{:else}<PlusIcon size="14" />{/if}
            Create
          </Button>
        </form>
      </section>

      <section aria-labelledby="tag-definitions-heading">
        <div class="section-heading">
          <div>
            <span>TAG DEFINITIONS</span>
            <strong id="tag-definitions-heading">{current.length} visible</strong>
          </div>
          <small>{currentTotal} total</small>
        </div>
        {#if current.length === 0}
          <p class="empty">No tags have been defined yet.</p>
        {:else}
          <div class="definition-list">
            {#each current as tag (tag.id)}
              <div class="definition-row">
                {#if editingID === tag.id}
                  <div class="rename-row">
                    <TextInput
                      bind:value={editName}
                      block
                      autofocus
                      ariaLabel={`Rename ${tag.name}`}
                      disabled={disabled || pending}
                      onkeydown={(event) => {
                        if (event.key === "Enter") {
                          event.preventDefault();
                          void saveRename();
                        } else if (event.key === "Escape") {
                          cancelRename();
                        }
                      }}
                    />
                    <IconButton
                      size="sm"
                      ariaLabel={`Save renamed tag ${tag.name}`}
                      disabled={disabled || pending || editName === ""}
                      onclick={() => void saveRename()}
                    >
                      {#if pending}<Spinner size={13} />{:else}<CheckIcon size="14" />{/if}
                    </IconButton>
                    <IconButton
                      size="sm"
                      ariaLabel={`Cancel renaming ${tag.name}`}
                      disabled={pending}
                      onclick={cancelRename}
                    >
                      <XIcon size="14" />
                    </IconButton>
                  </div>
                {:else}
                  <div class="definition-authority">
                    <div>
                      <Chip size="sm" tone="workspace" uppercase={false}>{tag.name}</Chip>
                      <span>{tag.assignment_count} assignment{tag.assignment_count === 1 ? "" : "s"}</span>
                    </div>
                    <code>{tag.id}</code>
                  </div>
                  <div class="definition-actions">
                    <IconButton
                      size="sm"
                      ariaLabel={`Rename tag ${tag.name}`}
                      title={`Rename ${tag.name}`}
                      disabled={disabled || pending}
                      onclick={() => beginRename(tag)}
                    >
                      <PencilIcon size="14" />
                    </IconButton>
                    <IconButton
                      size="sm"
                      ariaLabel={`Delete tag ${tag.name}`}
                      title={`Delete ${tag.name}`}
                      disabled={disabled || pending}
                      onclick={() => {
                        cancelRename();
                        deleting = tag;
                        failure = "";
                        notice = "";
                      }}
                    >
                      <Trash2Icon size="14" />
                    </IconButton>
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
        {#if currentTotal > current.length}
          <p class="bounded">
            Showing the first {current.length} of {currentTotal} definitions.
            Use the CLI or API for definitions outside this bounded catalog.
          </p>
        {/if}
      </section>

      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      {#if failure}<p class="failure" role="alert">{failure}</p>{/if}
    </div>
  {/if}
  {#snippet footer()}
    {#if deleting}
      <Button surface="soft" disabled={pending} onclick={() => (deleting = null)}>
        Keep tag
      </Button>
      <Button
        tone="danger"
        surface="solid"
        disabled={disabled || pending}
        onclick={() => void confirmDelete()}
      >
        {#if pending}<Spinner size={14} /> Deleting…{:else}<Trash2Icon size="14" /> Delete tag{/if}
      </Button>
    {:else}
      <Button surface="soft" disabled={pending} onclick={close}>Done</Button>
    {/if}
  {/snippet}
</Modal>

<style>
  .tag-catalog,
  .delete-confirmation {
    display: grid;
    gap: var(--space-5);
  }

  .catalog-heading,
  .delete-target {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    align-items: start;
    gap: var(--space-3);
    padding: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .catalog-heading > :global(svg) {
    margin-top: 2px;
    color: var(--accent-cyan);
  }

  .catalog-heading > div,
  .delete-target > div {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }

  .catalog-heading strong,
  .delete-target strong {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .catalog-heading span,
  .delete-target span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.4;
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
    gap: 2px;
  }

  .section-heading span,
  .section-heading small {
    color: var(--text-muted);
    font-size: 10px;
    letter-spacing: 0.08em;
  }

  .section-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .create-row,
  .rename-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-2);
  }

  .definition-list {
    display: grid;
    max-height: min(330px, 42vh);
    overflow-y: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .definition-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-3);
    min-height: 56px;
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }

  .definition-row:last-child {
    border-bottom: 0;
  }

  .definition-authority {
    display: grid;
    min-width: 0;
    gap: var(--space-2);
  }

  .definition-authority > div {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .definition-authority span,
  .definition-authority code,
  .delete-target code {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .definition-authority code,
  .delete-target code {
    overflow-wrap: anywhere;
  }

  .definition-actions {
    display: flex;
    gap: var(--space-1);
  }

  .rename-row {
    grid-column: 1 / -1;
    grid-template-columns: minmax(0, 1fr) auto auto;
  }

  .notice,
  .failure,
  .empty,
  .bounded,
  .delete-confirmation p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .notice {
    color: var(--accent-green);
  }

  .failure {
    padding: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent-red) 35%, transparent);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
    color: var(--accent-red);
  }

  .empty,
  .bounded,
  .boundary {
    color: var(--text-muted);
  }

  .delete-target > :global(svg) {
    margin-top: 2px;
    color: var(--accent-red);
  }

  @media (max-width: 640px) {
    .definition-row {
      align-items: start;
    }

    .definition-authority > div {
      align-items: start;
      flex-direction: column;
    }
  }
</style>
