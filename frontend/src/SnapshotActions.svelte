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
  import { Button, Disclosure, Modal, SegmentedControl, Spinner, Table } from "@kenn-io/kit-ui";
  import TagPicker from "./TagPicker.svelte";
  import type { Tag } from "./generated/docbank.js";
  import { randomUUID } from "./crypto.js";
  import type { SelectionTarget } from "./selection.js";
  import { ACTION_MAX_BYTES } from "./actionRecovery.js";
  import type { BatchTagRequest, BatchTagReceipt } from "./batch-tags.js";
  import { APIError } from "./api-transport.js";
  import { checkTagResults, type TagResult } from "./checkTagResults.js";
  import { TagChangeRun } from "./tagChangeRun.svelte.js";
  import { documentCount, tagChangeConsequence, tagChangeDone } from "./tagChangeText.js";
  import DocumentPager from "./DocumentPager.svelte";

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
  let request = $state<BatchTagRequest>();
  let recovering = $state(false);
  let checking = $state(false);
  let checked = $state(0);
  let results = $state<TagResult[]>();
  let retryResults: TagResult[] | undefined;
  let checkController: AbortController | undefined;
  let offset = $state(0);
  const remaining = $derived(results?.filter((row) => row.status === "remaining") ?? []);
  const unavailable = $derived(results?.filter((row) => row.status === "unavailable").length ?? 0);
  const matched = $derived(results?.filter((row) => row.status === "matched").length ?? 0);
  const run = new TagChangeRun({
    session: untrack(() => session),
    onauthfailure: (cause) => onauthfailure(cause),
    onconfirmed: (confirmed) => {
      if (retryResults?.some((row) => row.status === "unavailable")) {
        results = retryResults.map((row) =>
          row.status === "remaining" ? { ...row, status: "matched" } : row,
        );
      } else receipt = confirmed;
      retryResults = undefined;
      onchanged?.(confirmed);
    },
    onunknown: () => {
      recovering = true;
      onunknown?.();
    },
    unknownMessage:
      "Docbank couldn't confirm the tag change. Check the documents' tags before trying again.",
    staleMessage: "Some documents changed. Check their tags again.",
  });
  const applying = $derived(run.pending);
  const stale = $derived(run.stale);
  const failure = $derived(
    readFailure ||
      (run.stale && !recovering && run.failure
        ? "Some documents changed. Close, refresh, and select them again."
        : run.failure),
  );
  let fileInput = $state<HTMLInputElement>();
  // The selection is captured when the dialog opens; later clicks on the page do not change it.
  const captured = untrack(() => targets.map((target) => ({ ...target })));
  let alive = true;
  onDestroy(() => {
    alive = false;
    checkController?.abort();
    run.destroy();
  });

  const busy = $derived(disabled || preparing || applying || reading || checking);
  const locked = $derived(busy || stale || recovering || receipt !== undefined);
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
    request = {
      operation_id: randomUUID(),
      tag_id: tag.id,
      assign,
      nodes: captured.map((target) => ({ ...target })),
    };
    void run.submit(request);
  }

  async function check(): Promise<void> {
    if (!recovering || !request || checking || applying) return;
    checking = true;
    checked = 0;
    results = undefined;
    run.failure = "";
    const controller = new AbortController();
    checkController = controller;
    try {
      const rows = await checkTagResults(
        session,
        request,
        (value) => {
          if (alive) checked = value;
        },
        controller.signal,
      );
      if (!alive) return;
      results = rows;
      offset = 0;
    } catch (cause) {
      if (!alive) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else run.failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (alive) checking = false;
    }
  }

  function retry(): void {
    if (!request || !remaining.length || busy) return;
    const nodes = remaining.map((row) => ({ node_id: row.node_id, revision: row.revision! }));
    retryResults = results;
    results = undefined;
    void run.submit({
      ...request,
      operation_id: randomUUID(),
      nodes,
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
    {:else if results && tag}
      <p class="consequence">
        {#if results.length === 1 && unavailable === 0}
          The document {remaining.length === 0
            ? assign
              ? `has “${tag?.name}”`
              : `doesn't have “${tag?.name}”`
            : assign
              ? `doesn't have “${tag?.name}” yet`
              : `still has “${tag?.name}”`}.
        {:else if remaining.length === 0 && unavailable === 0}
          {assign
            ? `All ${documentCount(results.length)} have “${tag?.name}”.`
            : `None of the ${documentCount(results.length)} have “${tag?.name}”.`}
        {:else}
          {matched} of {documentCount(results.length)}
          {assign
            ? `${matched === 1 ? "has" : "have"} “${tag?.name}”`
            : `${matched === 1 ? "doesn't" : "don't"} have “${tag?.name}”`}. {remaining.length} still
          need the change.
        {/if}
      </p>
      {#if unavailable}<p class="status">
          {unavailable} couldn't be checked and are left out.
        </p>{/if}
    {:else if !failure}
      <p class="consequence" class:pending={!tag}>{consequence}</p>
    {/if}
    {#if applying || preparing || checking}
      <p class="status" role="status">
        <Spinner size={14} />
        {checking
          ? `Checked ${checked} of ${request?.nodes.length} documents…`
          : preparing
            ? `Preparing ${documentCount(count)}…`
            : "Applying…"}
      </p>
    {/if}
    {#if failure || errorMessage}<p class="alert" role="alert">{failure || errorMessage}</p>{/if}
    {#if results && tag}
      <Disclosure variant="plain" title="Review documents">
        <div class="target-list">
          <Table unsorted ariaLabel="Tag results" class="target-table" zebra={false} scroll="page">
            {#snippet header()}<th scope="col">Document</th><th scope="col">Status</th>{/snippet}
            {#snippet children()}
              {#each (results ?? []).slice(offset, offset + 50) as row (row.node_id)}
                <tr
                  ><td>{row.name}</td><td
                    >{row.status === "unavailable"
                      ? "Couldn't check"
                      : row.status === "matched"
                        ? assign
                          ? `Has “${tag?.name}”`
                          : `Doesn't have “${tag?.name}”`
                        : assign
                          ? `Missing “${tag?.name}”`
                          : `Still has “${tag?.name}”`}</td
                  ></tr
                >
              {/each}
            {/snippet}
          </Table>
        </div>
        <DocumentPager total={results.length} bind:offset />
      </Disclosure>
    {/if}
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
    {:else if recovering && request}
      {#if results}<span class="footer-start"
          ><Button size="sm" surface="soft" disabled={busy} onclick={() => void check()}
            >Check again</Button
          ></span
        >{/if}
      <Button surface="soft" disabled={applying} onclick={close}
        >{results && !remaining.length && !unavailable ? "Done" : "Close"}</Button
      >
      {#if results && remaining.length}
        <Button tone="info" surface="solid" disabled={busy} onclick={() => void retry()}
          >{assign ? "Add" : "Remove"} “{tag?.name}” {assign ? "to" : "from"}
          {documentCount(remaining.length)}</Button
        >
      {:else if !results}
        <Button
          tone="info"
          surface="solid"
          disabled={checking || applying}
          onclick={() => void check()}>{!run.unknown ? "Check again" : "Check tag results"}</Button
        >
      {/if}
    {:else if stale}
      <Button surface="soft" disabled={applying} onclick={close}>Close</Button>
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
  .tag-change :global(.kit-disclosure--plain > .kit-disclosure__content) {
    padding-block-end: 0;
  }
  .target-list {
    overflow: auto;
    max-height: 240px;
  }
  .target-list :global(.target-table .kit-table) {
    table-layout: fixed;
    font-size: var(--font-size-xs);
  }
  .target-list :global(.target-table th),
  .target-list :global(.target-table td) {
    padding: var(--space-2);
    text-align: left;
    overflow-wrap: anywhere;
    border-bottom: 1px solid var(--border-default);
  }
  .target-list :global(.target-table th:first-child),
  .target-list :global(.target-table td:first-child) {
    padding-inline-start: 0;
  }
  .target-list :global(.target-table .kit-table) {
    margin-block-end: 0;
  }
  .file {
    position: absolute;
    width: 1px;
    height: 1px;
    opacity: 0;
    pointer-events: none;
  }
</style>
