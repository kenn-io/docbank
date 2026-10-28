<script lang="ts">
  import type { Snippet } from "svelte";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import { Button, Spinner, TextInput } from "@kenn-io/kit-ui";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import { formatDate } from "./format.js";
  import {
    findBatesExportsByLabel,
    readBatesRecoveryDetail,
    type BatesCandidate,
    type BatesExport,
    type BatesRecoveryDetail,
  } from "./bates.js";

  interface Props {
    session: string;
    active: boolean;
    onauthfailure: (cause: unknown) => void;
    children: Snippet<[BatesExport]>;
  }
  let { session, active = $bindable(false), onauthfailure, children }: Props = $props();
  let input = $state("");
  let submitted = $state("");
  let items = $state<BatesCandidate[]>([]);
  let details = $state<Record<string, BatesRecoveryDetail>>({});
  let detailErrors = $state<Record<string, string>>({});
  let pending = $state<Record<string, boolean>>({});
  let loading = $state(false);
  let error = $state("");
  let nextCursor = $state("");
  let restart = $state(false);
  let controller = new AbortController();
  let seenCursors = new Set<string>();

  $effect(() => {
    void session;
    clear();
    return () => controller.abort();
  });

  function clear() {
    controller.abort();
    controller = new AbortController();
    seenCursors = new Set();
    input = submitted = error = nextCursor = "";
    items = [];
    details = {};
    detailErrors = {};
    pending = {};
    active = loading = restart = false;
  }

  function failure(cause: unknown): string {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      return "";
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  function search(label: string) {
    clear();
    input = submitted = label;
    active = true;
    void loadPage();
  }

  async function loadPage(cursor?: string) {
    const request = controller;
    loading = true;
    error = "";
    restart = false;
    try {
      const page = await findBatesExportsByLabel(session, submitted, request.signal, cursor);
      if (request.signal.aborted) return;
      if (page.next_cursor && seenCursors.has(page.next_cursor)) {
        throw new Error("The result cursor did not advance. Search again.");
      }
      if (page.next_cursor) seenCursors.add(page.next_cursor);
      const ids = new Set(items.map((item) => item.artifact_id));
      items = [
        ...items,
        ...page.items.filter((item) => {
          if (ids.has(item.artifact_id)) return false;
          ids.add(item.artifact_id);
          return true;
        }),
      ];
      nextCursor = page.next_cursor ?? "";
    } catch (cause) {
      if (request.signal.aborted) return;
      error = failure(cause);
      restart = true;
      nextCursor = "";
    } finally {
      if (!request.signal.aborted) loading = false;
    }
  }

  async function expand(item: BatesCandidate) {
    const id = item.artifact_id;
    if (details[id] || pending[id]) return;
    const request = controller;
    pending[id] = true;
    detailErrors[id] = "";
    try {
      const detail = await readBatesRecoveryDetail(session, item, request.signal);
      if (!request.signal.aborted) details[id] = detail;
    } catch (cause) {
      if (!request.signal.aborted) detailErrors[id] = failure(cause);
    } finally {
      if (!request.signal.aborted) pending[id] = false;
    }
  }
</script>

<section class="tool-section" aria-label="Find a production">
  <form
    onsubmit={(event) => {
      event.preventDefault();
      search(input);
    }}
  >
    <div class="tool-field">
      <label for="bates-number">Bates number</label>
      <TextInput id="bates-number" ariaLabel="Bates number" bind:value={input} block />
    </div>
    <Button type="submit" tone="info" surface="solid">Find</Button>
    {#if active}<Button onclick={clear}>Clear</Button>{/if}
  </form>
  {#if loading}<p class="tool-muted" role="status">
      <Spinner size={14} /> Finding productions…
    </p>{/if}
  {#if error}<p class="tool-error" role="alert">{error}</p>{/if}
  {#if restart}<div class="tool-actions">
      <Button onclick={() => search(submitted)}>Search again</Button>
    </div>{/if}
  {#if active && !loading && !error && !items.length}
    <p class="tool-muted" role="status">No retained PDF matches “{submitted}”.</p>
  {/if}
  {#if items.length}
    <ul class="tool-list" aria-label="Matching productions">
      {#each items as item (item.artifact_id)}
        <li>
          <details
            class="tool-disclosure"
            ontoggle={(event) => {
              if (event.currentTarget.open) void expand(item);
            }}
          >
            <summary>
              <ChevronRightIcon size={14} aria-hidden="true" />
              <span class="number">{submitted}<small>{formatDate(item.created_at)}</small></span>
              <span class="tool-muted"
                >Page {item.evidence.map((e) => e.output_page).join(", ")} of {item.page_count}</span
              >
            </summary>
            <div class="recovery-details">
              {#if item.evidence_truncated}<p class="tool-muted">
                  Showing some matching pages.
                </p>{/if}
              {#if pending[item.artifact_id]}<p role="status">
                  <Spinner size={14} /> Loading receipt…
                </p>{/if}
              {#if detailErrors[item.artifact_id]}
                <p class="tool-error" role="alert">{detailErrors[item.artifact_id]}</p>
                <Button onclick={() => void expand(item)}>Retry receipt</Button>
              {/if}
              {#if details[item.artifact_id]}
                {@const detail = details[item.artifact_id]!}
                <InspectorDetails
                  facts={[
                    { label: "Delivery", value: detail.kind },
                    { label: "Allocation", value: item.allocation_id, copy: true },
                  ]}
                />
                <dl class="namespace">
                  <div>
                    <dt>Prefix</dt>
                    <dd>{detail.namespace.prefix || "None"}</dd>
                  </div>
                  <div>
                    <dt>Suffix</dt>
                    <dd>{detail.namespace.suffix || "None"}</dd>
                  </div>
                </dl>
                {@render children(detail.receipt)}
                {#if detail.kind === "Numbered redaction"}
                  <p class="tool-muted">Masks and approved text are not rechecked on download.</p>
                {/if}
              {/if}
            </div>
          </details>
        </li>
      {/each}
    </ul>
  {/if}
  {#if nextCursor}<div class="tool-actions">
      <Button disabled={loading} onclick={() => void loadPage(nextCursor)}>Load more</Button>
    </div>{/if}
</section>

<style>
  form {
    display: flex;
    gap: var(--space-4);
    align-items: end;
  }
  .tool-field {
    flex: 1;
    min-width: 0;
  }
  .tool-list > li {
    display: block;
    padding: 0;
  }
  .tool-disclosure {
    border: 0;
  }
  summary {
    gap: var(--space-4);
  }
  .number {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  .number small {
    display: block;
    color: var(--text-muted);
  }
  .recovery-details {
    display: grid;
    gap: var(--space-5);
    padding: var(--space-5);
  }
  .namespace {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
    margin: 0;
  }
  dt {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  dd {
    margin: 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
