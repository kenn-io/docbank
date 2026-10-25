<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, Chip, IconButton, Spinner } from "@kenn-io/kit-ui";
  import CommandBox from "./CommandBox.svelte";
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import { type AuditVerifyReport } from "./generated/docbank.js";
  import { formatBytes } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let report = $state<AuditVerifyReport | null>(null);
  let loading = $state(true);
  let loaded = $state(false);
  let error = $state("");
  let controller: AbortController | null = null;
  let generation = 0;

  const metadataProblems = $derived(report?.metadata_problems ?? []);
  const contentProblems = $derived(report?.problems ?? []);
  const evidence = $derived(report?.evidence);
  const visibleScopes = $derived(evidence?.scopes.slice(0, 100) ?? []);
  const verified = $derived(
    Boolean(
      report?.enabled &&
      evidence &&
      metadataProblems.length === 0 &&
      contentProblems.length === 0 &&
      report.verified_blobs === report.protected_blobs,
    ),
  );

  const problemCount = $derived(metadataProblems.length + contentProblems.length);
  const summary = $derived<InspectorFact[]>(
    report
      ? [
          {
            label: "Protected",
            value: `${formatBytes(report.protected_bytes)} in ${report.protected_blobs} ${report.protected_blobs === 1 ? "file" : "files"}`,
          },
          {
            label: "Verified",
            value: `${report.verified_blobs} of ${report.protected_blobs} ${report.protected_blobs === 1 ? "file" : "files"} (SHA-256)`,
          },
        ]
      : [],
  );
  const identity = $derived<InspectorFact[]>(
    evidence
      ? [
          { label: "Vault ID", value: evidence.vault_id, copy: true },
          { label: "Lineage ID", value: evidence.lineage_id, copy: true },
          { label: "Allocation head", value: evidence.allocation_head, copy: true },
          { label: "Allocations", value: evidence.allocation_entry_count },
          { label: "Sequence", value: evidence.operation_sequence_high_water },
        ]
      : [],
  );

  onMount(() => {
    void refresh();
    return () => {
      generation += 1;
      controller?.abort();
    };
  });

  async function refresh(): Promise<void> {
    controller?.abort();
    const active = new AbortController();
    controller = active;
    const request = ++generation;
    loading = true;
    report = null;
    error = "";
    try {
      const next = await generated.verifyAudit({}, { session: session, signal: active.signal });
      if (request !== generation) return;
      report = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof DOMException && cause.name === "AbortError") return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) {
        loading = false;
        loaded = true;
        controller = null;
      }
    }
  }
</script>

<DrawerShell
  title="Protected folders"
  ariaLabel="Permanent audit verification"
  closeLabel="Close permanent audit verification"
  {onclose}
>
  {#snippet actions()}
    {#if loaded && !(report && !report.enabled)}
      <IconButton
        size="sm"
        ariaLabel="Run permanent audit verification again"
        disabled={loading}
        onclick={() => void refresh()}
      >
        <RefreshCwIcon size="14" aria-hidden="true" />
      </IconButton>
    {/if}
  {/snippet}

  <div class="verification">
    {#if loading && !report}
      <div class="loading"><Spinner size={16} /> Verifying…</div>
    {:else if error && !report}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if report && metadataProblems.length > 0 && !evidence}
      <div class="status" aria-live="polite">
        <Chip size="xs" tone="danger" dot>Failed</Chip>
        <span>Protected history needs attention</span>
      </div>
      {@render problems()}
    {:else if report && !report.enabled}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="empty">
        <p>No folders are protected. To protect one, run</p>
        <CommandBox label="audit enable command" command="docbank audit enable /folder" />
      </div>
    {:else if report && evidence}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="status" aria-live="polite">
        <Chip size="xs" tone={verified ? "success" : "danger"} dot>
          {verified ? "Complete" : "Failed"}
        </Chip>
        <span>
          {verified ? "Protected history and content agree" : "Protected history needs attention"}
        </span>
      </div>
      <InspectorDetails label="Verification summary" facts={summary} />
      {@render problems()}

      <details class="details">
        <summary>Details</summary>
        <div class="breakdown">
          <InspectorDetails label="Verified audit history" facts={identity} />
          <section aria-label="Verified audit scope heads">
            <h3>Scopes ({evidence.scopes.length})</h3>
            {#each visibleScopes as scope (scope.id)}
              <InspectorDetails
                label={`Scope ${scope.id}`}
                facts={[
                  { label: "Scope", value: scope.id, copy: true },
                  { label: "Entries", value: scope.entry_count },
                  { label: "Chain head", value: scope.chain_head, copy: true },
                ]}
              />
            {/each}
            {#if evidence.scopes.length > visibleScopes.length}
              <p class="muted">
                Showing the first {visibleScopes.length} of {evidence.scopes.length} scopes. Run
                <code>docbank audit verify --json</code> for all of them.
              </p>
            {/if}
          </section>
        </div>
      </details>
    {/if}
  </div>
</DrawerShell>

{#snippet problems()}
  {#if problemCount > 0}
    <section class="problems" aria-label="Audit verification problems">
      <h3>{problemCount} {problemCount === 1 ? "problem" : "problems"}</h3>
      {#each metadataProblems as problem}
        <p><strong>Metadata</strong> {problem}</p>
      {/each}
      {#each contentProblems as problem}
        <p>
          <strong>{problem.problem}</strong>
          <code>{problem.hash}</code>
        </p>
      {/each}
    </section>
  {/if}
{/snippet}

<style>
  .verification {
    display: grid;
    align-content: start;
    gap: var(--space-5);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }

  .loading,
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .loading {
    color: var(--text-secondary);
  }

  .load-error {
    display: grid;
    justify-items: start;
    gap: var(--space-4);
  }

  .load-error p,
  .error {
    margin: 0;
    color: var(--accent-red);
  }

  .empty {
    display: grid;
    justify-items: start;
    gap: var(--space-3);
  }

  .empty p,
  .muted {
    margin: 0;
    color: var(--text-secondary);
  }

  h3 {
    margin: 0;
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  .verification :global(.inspector-details > div) {
    grid-template-columns: 8rem minmax(0, 1fr);
  }

  .problems {
    display: grid;
    gap: var(--space-2);
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent-red) 45%, var(--border-muted));
    border-radius: var(--radius-lg);
    background: color-mix(in srgb, var(--accent-red) 7%, var(--bg-surface));
  }

  .problems p {
    display: grid;
    gap: var(--space-1);
    margin: 0;
    color: var(--text-secondary);
  }

  code {
    overflow-wrap: anywhere;
    font-size: var(--font-size-sm);
  }

  summary {
    cursor: pointer;
    width: fit-content;
    color: var(--text-secondary);
  }

  summary:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }

  .breakdown,
  .breakdown section {
    display: grid;
    gap: var(--space-4);
  }

  .breakdown {
    margin-top: var(--space-4);
    gap: var(--space-5);
  }

  @media (max-width: 640px) {
    .verification :global(.inspector-details > div) {
      grid-template-columns: 1fr;
      gap: var(--space-1);
    }
  }
</style>
