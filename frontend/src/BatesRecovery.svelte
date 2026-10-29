<script lang="ts">
  import type { Snippet } from "svelte";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import { Button, Spinner, TextInput } from "@kenn-io/kit-ui";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { formatBytes, formatDate } from "./format.js";
  import {
    findBatesExportsByLabel,
    readBatesRecoveryDetail,
    validBatesLabel,
    type BatesCandidate,
    type BatesExport,
    type BatesRecoveryDetail,
  } from "./bates.js";

  interface Props {
    session: string;
    active: boolean;
    unauthorized: (cause: unknown) => boolean;
    children: Snippet<[BatesExport]>;
  }
  let { session, active = $bindable(false), unauthorized, children }: Props = $props();
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
  const canSearch = $derived(validBatesLabel(input));

  $effect(() => {
    void session;
    clear();
    return () => {
      controller.abort();
      active = false;
    };
  });

  function clear() {
    controller.abort();
    controller = new AbortController();
    input = submitted = error = nextCursor = "";
    items = [];
    details = {};
    detailErrors = {};
    pending = {};
    active = loading = restart = false;
  }

  function failure(cause: unknown): string | undefined {
    if (unauthorized(cause)) {
      controller.abort();
      return;
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  function search(label: string) {
    if (!validBatesLabel(label)) return;
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
      if (!cursor && items.length === 1 && !nextCursor) void expand(items[0]!);
    } catch (cause) {
      if (request.signal.aborted) return;
      const message = failure(cause);
      if (message === undefined) return;
      error = message;
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
      if (request.signal.aborted) return;
      const message = failure(cause);
      if (message !== undefined) detailErrors[id] = message;
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
    <Button type="submit" tone="info" surface="solid" disabled={!canSearch}>Find</Button>
    {#if active}<Button onclick={clear}>Clear</Button>{/if}
  </form>
  {#if input && !canSearch}<p class="tool-muted">Use at most 266 characters.</p>{/if}
  {#if input !== input.trim()}<p class="tool-muted">
      Leading or trailing spaces are included in the search.
    </p>{/if}
  {#if loading && !nextCursor}<p class="tool-muted" role="status">
      <Spinner size={14} /> Finding productions…
    </p>{/if}
  {#if error}<p class="tool-error" role="alert">{error}</p>{/if}
  {#if restart}<div class="tool-actions">
      <Button onclick={() => search(submitted)}>Search again</Button>
    </div>{/if}
  {#if active && !loading && !error && !items.length}
    <p class="tool-muted" role="status">No retained PDF matches “{submitted}”.</p>
    <p class="tool-muted">Numbers must match exactly, including prefix and leading zeros.</p>
  {/if}
  {#if items.length}
    <ul class="tool-list" aria-label="Matching productions">
      {#each items as item (item.artifact_id)}
        <li>
          <details
            class="tool-disclosure"
            open={items.length === 1 && !nextCursor}
            ontoggle={(event) => {
              if (event.currentTarget.open) void expand(item);
            }}
          >
            <summary>
              <ChevronRightIcon size={14} aria-hidden="true" />
              <span class="result-title">
                Page {item.evidence.map((e) => e.output_page).join(", ")} of {item.page_count}
                <small>{formatDate(item.created_at)} · {formatBytes(item.size)}</small>
              </span>
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
                    {
                      label: "Bates range",
                      value: `${detail.receipt.pages[0]!.label}–${detail.receipt.pages.at(-1)!.label}`,
                    },
                    { label: "PDF SHA-256", value: detail.receipt.blob_sha256, copy: true },
                    { label: "Allocation", value: item.allocation_id, copy: true },
                  ]}
                />
                <div class="recovery-actions">
                  {@render children(detail.receipt)}
                  <p class="tool-muted">
                    Download checked against its retained record{detail.kind ===
                    "Numbered redaction"
                      ? "; masks and approved text aren’t rechecked."
                      : "."}
                  </p>
                </div>
              {/if}
            </div>
          </details>
        </li>
      {/each}
    </ul>
  {/if}
  {#if nextCursor}<div class="tool-actions">
      <Button disabled={loading} onclick={() => void loadPage(nextCursor)}>
        {#if loading}<span aria-hidden="true"><Spinner size={14} /></span> Loading more…{:else}Load
          more{/if}
      </Button>
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
  .result-title {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  .result-title small {
    display: block;
    color: var(--text-muted);
  }
  .recovery-details {
    display: grid;
    gap: var(--space-5);
    padding: var(--space-5);
  }
  .recovery-actions {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }
  .recovery-actions p {
    margin: 0;
  }
</style>
