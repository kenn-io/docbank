<script lang="ts">
  import { tick, untrack } from "svelte";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import { Button, Chip, IconButton, Spinner } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import DocumentReader from "./DocumentReader.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import {
    getContract,
    getNode,
    type Contract,
    type ContractDocument,
  } from "./generated/docbank.js";
  import {
    contractDocumentSource,
    contractStatusLabel,
    contractTypeLabel,
    documentRoleLabel,
    formatCivilDate,
    statusTone,
  } from "./contracts.js";
  import { formatBytes, formatDate } from "./format.js";
  import type { RelatedSelectedSource } from "./selectedSource.js";

  let {
    session,
    contractID,
    asOf,
    onauthfailure,
    onback,
  }: {
    session: string;
    contractID: string;
    asOf: string;
    onauthfailure: (cause: unknown) => void;
    onback: (id: string) => void;
  } = $props();

  let contract = $state<Contract | null>(null);
  let loading = $state(true);
  let error = $state("");
  let missing = $state(false);
  let names = $state<Record<number, string>>({});
  let reader = $state<RelatedSelectedSource>();
  let opening = $state("");
  let openError = $state("");
  let trigger = "";
  let controller: AbortController | undefined;
  let openController: AbortController | undefined;

  $effect(() => {
    untrack(() => void load());
    return () => {
      controller?.abort();
      openController?.abort();
    };
  });

  function authFailure(cause: unknown): boolean {
    if (!(cause instanceof APIError) || cause.status !== 401) return false;
    onauthfailure(cause);
    return true;
  }

  async function load(): Promise<void> {
    controller?.abort();
    const current = (controller = new AbortController());
    loading = true;
    error = "";
    missing = false;
    try {
      const value = await getContract(contractID, asOf ? { as_of: asOf } : undefined, {
        session,
        signal: current.signal,
      });
      if (current.signal.aborted) return;
      contract = value;
      void loadNames(value.documents, current.signal);
    } catch (cause) {
      if (current.signal.aborted || authFailure(cause)) return;
      if (cause instanceof APIError && cause.status === 404) missing = true;
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!current.signal.aborted) loading = false;
    }
  }

  // Links record no name, so the detail looks up names for available links
  // only; a failed lookup keeps the id:N selector.
  async function loadNames(documents: ContractDocument[], signal: AbortSignal): Promise<void> {
    const ids = [...new Set(documents.filter((d) => d.available).map((d) => d.node_id))];
    await Promise.all(
      ids.map(async (id) => {
        try {
          const node = await getNode(id, { session, signal });
          if (!signal.aborted) names = { ...names, [id]: node.name };
        } catch (cause) {
          if (!signal.aborted) authFailure(cause);
        }
      }),
    );
  }

  function linkKey(link: ContractDocument): string {
    return `${link.role}:${link.node_id}:${link.content_version_id}`;
  }
  function linkName(link: ContractDocument): string {
    return names[link.node_id] ?? `id:${link.node_id}`;
  }

  async function open(link: ContractDocument): Promise<void> {
    openController?.abort();
    const current = (openController = new AbortController());
    trigger = linkKey(link);
    opening = linkKey(link);
    openError = "";
    try {
      const source = await contractDocumentSource(session, link, current.signal);
      if (!current.signal.aborted) reader = source;
    } catch (cause) {
      if (current.signal.aborted || authFailure(cause)) return;
      openError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!current.signal.aborted) opening = "";
    }
  }

  function closeReader(): void {
    reader = undefined;
    void tick().then(() =>
      document
        .querySelector<HTMLElement>(`[data-contract-link="${CSS.escape(trigger)}"] button`)
        ?.focus(),
    );
  }

  const terms = $derived(
    contract
      ? [
          { label: "Summary", value: contract.summary },
          { label: "Purpose", value: contract.purpose },
          { label: "Consideration", value: contract.consideration },
          { label: "Termination", value: contract.termination },
        ].filter((term) => term.value !== "")
      : [],
  );
</script>

{#snippet backButton()}
  <IconButton
    size="sm"
    ariaLabel="All contracts"
    title="All contracts"
    onclick={() => onback(contractID)}><ArrowLeftIcon size="14" aria-hidden="true" /></IconButton
  >
{/snippet}

{#if loading}
  <div class="title">{@render backButton()}</div>
  <p class="tool-muted" role="status"><Spinner size={14} /> Loading contract…</p>
{:else if missing}
  <div class="title">{@render backButton()}</div>
  <div class="tool-heading">
    <p class="tool-muted">This contract no longer exists.</p>
    <Button size="sm" onclick={() => onback(contractID)}>All contracts</Button>
  </div>
{:else if error || !contract}
  <div class="title">{@render backButton()}</div>
  <div class="tool-heading">
    <p class="tool-error" role="alert">{error}</p>
    <Button size="sm" onclick={() => void load()}>Try again</Button>
  </div>
{:else}
  {@const current = contract}
  <section class="tool-section" aria-labelledby="contract-heading">
    <div class="tool-heading">
      <div class="title">
        {@render backButton()}
        <h3 id="contract-heading">{current.title}</h3>
        <Chip size="xs" tone={statusTone(current.effective_status)}
          >{contractStatusLabel(current.effective_status)}</Chip
        >
      </div>
    </div>
    <InspectorDetails
      label="Contract details"
      facts={[
        { label: "Type", value: contractTypeLabel(current.contract_type) },
        { label: "Status", value: contractStatusLabel(current.effective_status) },
        { label: "Starts", value: formatCivilDate(current.starts_on) },
        { label: "Expires", value: formatCivilDate(current.expires_on) },
        { label: "Signed", value: formatCivilDate(current.signed_on) },
        { label: "Updated", value: formatDate(current.updated_at) },
      ]}
    />
  </section>

  <section class="tool-section" aria-labelledby="contract-parties-heading">
    <h3 id="contract-parties-heading">Parties</h3>
    {#if current.parties.length === 0}
      <p class="tool-muted">No parties linked.</p>
    {:else}
      <ul class="tool-list">
        {#each current.parties as party (`${party.role}:${party.party_id}`)}
          <li>
            <div class="tool-row-main">
              <strong>{party.display_name}</strong>
              <span>{party.role === "own" ? "Own entity" : "Counterparty"}</span>
            </div>
            {#if party.retired_at}<Chip size="xs" tone="neutral">Retired</Chip>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <section class="tool-section" aria-labelledby="contract-terms-heading">
    <h3 id="contract-terms-heading">Terms</h3>
    {#if terms.length === 0}
      <p class="tool-muted">No terms recorded.</p>
    {:else}
      <dl class="terms">
        {#each terms as term (term.label)}
          <div>
            <dt>{term.label}</dt>
            <dd>{term.value}</dd>
          </div>
        {/each}
      </dl>
    {/if}
  </section>

  <section class="tool-section" aria-labelledby="contract-documents-heading">
    <h3 id="contract-documents-heading">Documents</h3>
    {#if current.documents.length === 0}
      <p class="tool-muted">No documents linked.</p>
    {:else}
      <ul class="tool-list">
        {#each current.documents as link (linkKey(link))}
          <li>
            <div class="tool-row-main">
              <strong>{linkName(link)}</strong>
              <span class="link-state">
                {documentRoleLabel(link.role)}
                {#if !link.available}
                  <Chip size="xs" tone="warning">Unavailable</Chip>
                  <span class="tool-muted"
                    >sha256 {link.blob_sha256.slice(0, 12)} · {formatBytes(link.size)}</span
                  >
                {:else if !link.current}
                  <Chip size="xs" tone="info">Older version</Chip>
                {/if}
              </span>
            </div>
            <span data-contract-link={linkKey(link)}
              ><Button
                size="sm"
                disabled={!link.available || opening !== ""}
                ariaLabel={`Open ${documentRoleLabel(link.role)} ${linkName(link)}`}
                onclick={() => void open(link)}
                >{opening === linkKey(link) ? "Opening…" : "Open"}</Button
              ></span
            >
          </li>
        {/each}
      </ul>
      {#if openError}<p class="tool-error" role="alert">{openError}</p>{/if}
    {/if}
  </section>
{/if}

{#if reader}
  {#key reader.key}<DocumentReader
      {session}
      source={reader}
      authorizationRevision={reader.mutationRevision}
      {onauthfailure}
      onclose={closeReader}
    />{/key}
{/if}

<style>
  .title {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }
  .title h3 {
    margin: 0;
    font-size: var(--font-size-md);
    overflow-wrap: anywhere;
  }
  .link-state {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }
  .terms {
    display: grid;
    gap: var(--space-4);
    margin: 0;
  }
  .terms dt {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .terms dd {
    margin: 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
