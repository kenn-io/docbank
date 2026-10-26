<script lang="ts">
  import { untrack } from "svelte";
  import {
    Button,
    Modal,
    SelectDropdown,
    Spinner,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { canonicalQuery, parseQuery, type Query } from "./query.js";
  import { previewQuery, querySelection, type QueryPreview } from "./queryPreview.js";
  import type { ProcessingProfileSummary } from "./generated/docbank.js";
  import type { SnapshotOptions } from "./snapshots.js";

  interface Props {
    session: string;
    open?: boolean;
    showExpression?: boolean;
    profiles?: ProcessingProfileSummary[];
    ondirty?: (dirty: boolean, state: { invalid: boolean; options: SnapshotOptions }) => void;
    oncancel: () => void;
    query: Query;
    profile?: string;
    initialPageSize?: 50 | 100 | 250;
    runError?: string;
    onchange: (query: Query) => void;
    onrun: (query: Query, options: SnapshotOptions) => void;
    onsave: () => void;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }
  let {
    session,
    open = true,
    showExpression = false,
    profiles = [],
    ondirty,
    oncancel,
    query,
    profile = "",
    initialPageSize = 100,
    runError = "",
    onchange,
    onrun,
    onsave,
    onclose,
    onauthfailure,
  }: Props = $props();
  let draft = $state<Query>(untrack(() => parseQuery(canonicalQuery(query))));
  let facets = $state(untrack(() => JSON.stringify(query.filters, null, 2)));
  let schemaError = $state("");
  let failure = $state("");
  let position = $state<unknown>();
  let preview = $state<QueryPreview>();
  let pending = $state(false);
  let expression = $state<HTMLTextAreaElement>();
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let controller: AbortController | undefined;
  let currentIdentity = "";
  let currentText = untrack(() => query.text);
  let executionProfile = $state(untrack(() => profile));
  let pageSize = $state(untrack(() => initialPageSize));
  const originalPageSize = untrack(() => initialPageSize);
  const initialQuery = untrack(() => canonicalQuery(query));
  const initialProfile = untrack(() => profile);
  const sortOptions: SelectDropdownOption[] = [
    { value: "name", label: "Name" },
    { value: "path", label: "Path" },
    { value: "modified_at", label: "Modified" },
    { value: "size", label: "Size" },
    { value: "media_type", label: "File type" },
  ];
  const profileOptions = $derived([
    { value: "", label: "Default" },
    ...[...new Set([...profiles.map((item) => item.name), ...(profile ? [profile] : [])])]
      .sort()
      .map((name) => ({ value: name, label: name })),
  ]);
  const directionOptions: SelectDropdownOption[] = [
    { value: "asc", label: "Ascending" },
    { value: "desc", label: "Descending" },
  ];
  const selection = $derived(querySelection(draft.text, position));
  const expressionError = $derived(schemaError.startsWith("query text") ? schemaError : failure);
  const filtersError = $derived(schemaError.startsWith("query text") ? "" : schemaError);
  const profileError = $derived(
    new TextEncoder().encode(executionProfile.trim()).length > 128
      ? "Processing profile exceeds 128 bytes."
      : "",
  );
  const executionError = $derived(
    draft.mode !== "lexical"
      ? "This saved query uses an unsupported search method."
      : draft.sort.field === "relevance"
        ? "Choose a sort other than Relevance."
        : "",
  );
  const requestedFacets: SnapshotOptions["facets"] = [
    "collections",
    "tags",
    "media_family",
    "extension",
    "modified",
    "size",
    "text_coverage",
    "duplicates",
  ];

  function invalidate() {
    generation++;
    if (timer !== undefined) clearTimeout(timer);
    controller?.abort();
    timer = undefined;
    controller = undefined;
    preview = undefined;
    failure = "";
    position = undefined;
    pending = false;
  }

  function validate(emit: boolean) {
    invalidate();
    schemaError = "";
    let valid: Query;
    try {
      // Preserve raw facet JSON until the strict shared codec checks duplicate
      // keys and numeric lexemes. JSON.parse here would erase that evidence.
      const { filters: _filters, ...rest } = draft;
      valid = parseQuery(`${JSON.stringify(rest).slice(0, -1)},"filters":${facets}}`);
      draft = valid;
      currentIdentity = canonicalQuery(valid);
    } catch (cause) {
      schemaError = cause instanceof Error ? cause.message : String(cause);
      currentIdentity = "";
      if (emit) notifyDirty();
      return;
    }
    if (emit) {
      onchange(valid);
      notifyDirty();
    }
    const requestGeneration = generation;
    const identity = currentIdentity;
    const requestSession = session;
    pending = true;
    timer = setTimeout(() => {
      timer = undefined;
      const abort = new AbortController();
      controller = abort;
      const current = () =>
        requestGeneration === generation && identity === currentIdentity && !abort.signal.aborted;
      void previewQuery(requestSession, valid, abort.signal)
        .then((result) => {
          if (current()) preview = result;
        })
        .catch((cause: unknown) => {
          if (!current()) return;
          if (cause instanceof APIError && cause.status === 401) {
            onauthfailure(cause);
            return;
          }
          failure = cause instanceof Error ? cause.message : String(cause);
          if (cause instanceof APIError && cause.code === "invalid_query")
            position = cause.position;
        })
        .finally(() => {
          if (current()) pending = false;
        });
    }, 250);
  }

  $effect(() => {
    const canonical = canonicalQuery(query);
    const activeSession = session;
    const visible = open;
    untrack(() => {
      if (!visible) {
        invalidate();
        return;
      }
      if (canonical !== currentIdentity) {
        const current = parseQuery(canonical);
        if (schemaError) {
          if (current.text !== currentText) draft = { ...draft, text: current.text };
        } else {
          draft = current;
          facets = JSON.stringify(draft.filters, null, 2);
        }
      }
      currentText = query.text;
      if (activeSession) validate(false);
      else invalidate();
    });
    return () => invalidate();
  });

  function currentOptions(): SnapshotOptions {
    const selectedProfile = executionProfile.trim();
    return {
      ...(selectedProfile ? { profile: selectedProfile } : {}),
      page_size: pageSize,
      facets: requestedFacets,
    };
  }

  function notifyDirty() {
    const invalid = schemaError !== "" || profileError !== "";
    ondirty?.(
      invalid ||
        canonicalQuery(draft) !== initialQuery ||
        executionProfile !== initialProfile ||
        pageSize !== originalPageSize,
      { invalid, options: currentOptions() },
    );
  }

  function close() {
    invalidate();
    onclose();
  }
  function cancel() {
    invalidate();
    oncancel();
  }
  function focusError() {
    if (!selection || !expression) return;
    expression.focus();
    expression.setSelectionRange(selection.start, selection.end);
  }

  function run(): void {
    if (schemaError || profileError || executionError) return;
    onrun(draft, currentOptions());
  }
</script>

{#if open}
  <Modal
    title="Filters and sort"
    width="560px"
    maxWidth="calc(100vw - 32px)"
    closable={false}
    onclose={close}
  >
    <div class="query-bar">
      {#if showExpression}
        <div class="field">
          <label for="query-expression"
            >{draft.syntax === "advanced" ? "Query syntax" : "Plain words"}</label
          >
          <textarea
            id="query-expression"
            bind:this={expression}
            value={draft.text}
            aria-invalid={expressionError ? "true" : undefined}
            aria-describedby={expressionError ? "query-expression-error" : undefined}
            rows="3"
            spellcheck="false"
            oninput={(event) => {
              draft = { ...draft, text: event.currentTarget.value };
              validate(true);
            }}></textarea>
          {#if expressionError}<p id="query-expression-error" class="error" role="alert">
              {expressionError}
            </p>{/if}
          {#if selection}<Button size="sm" onclick={focusError}>Focus query error</Button>{/if}
          {#if draft.syntax === "advanced"}<details class="syntax-help">
              <summary>Syntax help</summary>
              <p>Phrases, AND, OR, NOT, parentheses, suffix *, and NEAR/5.</p>
              <code>name:manual OR collection:archive</code>
              <p>References: <code>tag:</code>, <code>collection:</code>, <code>saved:</code>.</p>
            </details>{/if}
        </div>
      {:else if expressionError}<p class="error" role="alert">{expressionError}</p>{/if}
      <div class="fields">
        <div class="field">
          <span class="field-label">Sort by</span>
          <div class="controls">
            <SelectDropdown
              value={draft.sort.field}
              options={sortOptions}
              title="Sort by"
              onchange={(value) => {
                draft = {
                  ...draft,
                  sort: { ...draft.sort, field: value as Query["sort"]["field"] },
                };
                validate(true);
              }}
            />
            <SelectDropdown
              value={draft.sort.direction}
              options={directionOptions}
              title="Direction"
              onchange={(value) => {
                draft = {
                  ...draft,
                  sort: { ...draft.sort, direction: value as Query["sort"]["direction"] },
                };
                validate(true);
              }}
            />
          </div>
          {#if executionError}<p class="error" role="alert">
              {executionError}
            </p>{/if}
        </div>
        <div class="field">
          <span class="field-label">Documents per page</span>
          <SelectDropdown
            title="Documents per page"
            value={String(pageSize)}
            options={[50, 100, 250].map((size) => ({ value: String(size), label: String(size) }))}
            onchange={(value) => {
              pageSize = Number(value) as typeof pageSize;
              notifyDirty();
            }}
          />
        </div>
      </div>
      <details open={filtersError !== "" || profileError !== ""}>
        <summary>More</summary>
        <div class="advanced-fields">
          {#if profiles.length > 1}
            <div class="field">
              <span class="field-label">Processing profile</span>
              <SelectDropdown
                title="Processing profile"
                value={executionProfile}
                options={profileOptions}
                onchange={(value) => {
                  executionProfile = value;
                  notifyDirty();
                }}
              />
            </div>
          {/if}
          <div class="field">
            <label for="query-facets">Filters JSON</label>
            <textarea
              id="query-facets"
              value={facets}
              rows="4"
              spellcheck="false"
              oninput={(event) => {
                facets = event.currentTarget.value;
                validate(true);
              }}></textarea>
            {#if filtersError}<p class="error" role="alert">{filtersError}</p>{/if}
          </div>
          {#if profileError}<p class="error" role="alert">{profileError}</p>{/if}
        </div>
      </details>
      {#if runError}<p class="error" role="alert">{runError}</p>{/if}
    </div>
    {#snippet footer()}
      <div class="validation" aria-live="polite">
        {#if pending}<span role="status" aria-label="Checking query"><Spinner size={14} /></span>
        {:else if preview}<span role="status" aria-label="Query valid" class="kit-sr-only"
          ></span>{/if}
      </div>
      <Button onclick={cancel}>Cancel</Button>
      <Button disabled={schemaError !== "" || profileError !== ""} onclick={onsave}>Save…</Button>
      <Button
        tone="info"
        surface="solid"
        disabled={schemaError !== "" || profileError !== "" || executionError !== ""}
        onclick={run}>Search</Button
      >
    {/snippet}
  </Modal>
{/if}

<style>
  .query-bar,
  .advanced-fields {
    display: grid;
    gap: var(--space-4);
  }
  .field {
    display: grid;
    align-content: start;
    gap: var(--space-2);
    min-width: 0;
  }
  .fields,
  .controls {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .fields {
    gap: var(--space-5);
  }
  .controls {
    gap: var(--space-2);
  }
  label,
  .field-label {
    font-size: var(--font-size-sm);
    font-weight: 500;
  }
  textarea {
    box-sizing: border-box;
    width: 100%;
    padding: var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
  }
  textarea {
    resize: vertical;
  }
  #query-facets {
    font-family: var(--font-mono);
  }
  summary {
    cursor: pointer;
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  .advanced-fields {
    margin-top: var(--space-3);
  }
  p {
    margin: 0;
    font-size: var(--font-size-sm);
  }
  .syntax-help p,
  .syntax-help code {
    margin-top: var(--space-2);
  }
  .error {
    color: var(--accent-red);
    overflow-wrap: anywhere;
  }
  .validation {
    margin-right: auto;
    align-self: center;
    color: var(--text-muted);
  }
</style>
