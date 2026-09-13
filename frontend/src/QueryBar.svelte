<script lang="ts">
  import { untrack } from "svelte";
  import { Button, Chip, SelectDropdown, Spinner, type SelectDropdownOption } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { canonicalQuery, parseQuery, type Query } from "./query.js";
  import { previewQuery, querySelection, type QueryPreview } from "./queryPreview.js";

  interface Props {
    session: string;
    query: Query;
    onchange: (query: Query) => void;
    onsave: () => void;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, query, onchange, onsave, onclose, onauthfailure }: Props = $props();
  let draft = $state<Query>(untrack(() => parseQuery(canonicalQuery(query))));
  let facets = $state(untrack(() => JSON.stringify(query.filters, null, 2)));
  let schemaError = $state("");
  let failure = $state("");
  let position = $state<unknown>();
  let preview = $state<QueryPreview>();
  let pending = $state(false);
  let expression: HTMLTextAreaElement;
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let controller: AbortController | undefined;
  let currentIdentity = "";
  const syntaxOptions: SelectDropdownOption[] = [{value:"simple",label:"Simple"},{value:"advanced",label:"Advanced"}];
  const modeOptions: SelectDropdownOption[] = ["lexical","semantic","hybrid"].map((value) => ({value,label:value}));
  const sortOptions: SelectDropdownOption[] = ["name","path","modified_at","size","media_type","relevance"].map((value) => ({value,label:value}));
  const directionOptions: SelectDropdownOption[] = [{value:"asc",label:"Ascending"},{value:"desc",label:"Descending"}];
  const selection = $derived(querySelection(draft.text, position));

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
      return;
    }
    if (emit) onchange(valid);
    const requestGeneration = generation;
    const identity = currentIdentity;
    const requestSession = session;
    pending = true;
    timer = setTimeout(() => {
      timer = undefined;
      const abort = new AbortController();
      controller = abort;
      const current = () => requestGeneration === generation && identity === currentIdentity && !abort.signal.aborted;
      void previewQuery(requestSession, valid, abort.signal).then((result) => {
        if (current()) preview = result;
      }).catch((cause: unknown) => {
        if (!current()) return;
        if (cause instanceof APIError && cause.status === 401) { onauthfailure(cause); return; }
        failure = cause instanceof Error ? cause.message : String(cause);
        if (cause instanceof APIError && cause.code === "invalid_query") position = cause.position;
      }).finally(() => {
        if (current()) pending = false;
      });
    }, 250);
  }

  $effect(() => {
    const canonical = canonicalQuery(query);
    const activeSession = session;
    untrack(() => {
      draft = parseQuery(canonical);
      facets = JSON.stringify(draft.filters, null, 2);
      if (activeSession) validate(false);
      else invalidate();
    });
    return () => invalidate();
  });

  function close() { invalidate(); onclose(); }
  function focusError() {
    if (!selection) return;
    expression.focus();
    expression.setSelectionRange(selection.start, selection.end);
  }
</script>

<section class="query-bar" aria-label="Query editor">
  <div class="heading"><h2>Query editor</h2><Button size="sm" disabled={schemaError !== ""} onclick={close}>Close query editor</Button></div>
  <p>Edit complete query intent. This draft does not change the live results below.</p>
  <div class="controls">
    <SelectDropdown value={draft.syntax} options={syntaxOptions} title="Query syntax" onchange={(value) => { draft = {...draft,syntax:value as Query["syntax"]}; validate(true); }} />
    <SelectDropdown value={draft.mode} options={modeOptions} title="Query mode" onchange={(value) => { draft = {...draft,mode:value as Query["mode"]}; validate(true); }} />
    <SelectDropdown value={draft.sort.field} options={sortOptions} title="Query sort" onchange={(value) => { draft = {...draft,sort:{...draft.sort,field:value as Query["sort"]["field"]}}; validate(true); }} />
    <SelectDropdown value={draft.sort.direction} options={directionOptions} title="Query direction" onchange={(value) => { draft = {...draft,sort:{...draft.sort,direction:value as Query["sort"]["direction"]}}; validate(true); }} />
  </div>
  <label for="query-expression">Query expression</label>
  <textarea id="query-expression" bind:this={expression} value={draft.text} rows="2" spellcheck="false"
    oninput={(event) => { draft = {...draft,text:event.currentTarget.value}; validate(true); }}></textarea>
  <details>
    <summary>Syntax help</summary>
    <p>Simple syntax treats whitespace-separated words as literal prefixes. Advanced syntax supports quoted phrases, parentheses, AND, OR, NOT, suffix * prefixes, and NEAR/5 proximity.</p>
    <p>Keep field operands in their Boolean scope, for example <code>name:manual OR collection:archive</code>. References use <code>tag:</code>, <code>collection:</code>, or <code>saved:</code>. The server validates supported fields and values; unknown exclusions block validation.</p>
  </details>
  <label for="query-facets">Structured facets JSON</label>
  <textarea id="query-facets" value={facets} rows="3" spellcheck="false"
    oninput={(event) => { facets = event.currentTarget.value; validate(true); }}></textarea>
  <div class="summaries" aria-label="Structured facet summaries">
    {#each Object.entries(draft.filters).filter(([, value]) => value !== undefined) as [key, value]}<Chip>{key}: {JSON.stringify(value)}</Chip>{/each}
  </div>
  {#if schemaError}<p role="alert">{schemaError} Correct the draft or choose Discard query draft before closing.</p>{/if}
  <div aria-live="polite">
    {#if pending}<span class="checking"><Spinner size={14} /> Validating query…</span>{/if}
    {#if preview}
      <p>Query validated. Execution is unavailable.</p>
      <div class="summaries" aria-label="Resolved query dependencies">
        {#each preview.dependencies as dependency (`${dependency.kind}:${dependency.id}`)}<Chip>{dependency.kind}: {dependency.id} · revision {dependency.revision}</Chip>{/each}
      </div>
    {/if}
    {#if failure}<p role="alert">{failure}</p>{/if}
  </div>
  {#if selection}<Button size="sm" onclick={focusError}>Focus query error</Button>{/if}
  <div class="controls">
    <Button disabled={schemaError !== ""} onclick={onsave}>Save query draft</Button>
    <Button disabled>Run query</Button>
    <span>The available live-search endpoint cannot honor this complete query. Validation never drops constraints or runs a broader search.</span>
  </div>
</section>

<style>
  .query-bar { display: grid; gap: var(--space-3); padding: var(--space-4); border-bottom: 1px solid var(--border-default); background: var(--bg-raised); }
  .query-bar p, h2 { margin: 0; }
  h2 { font-size: var(--font-size-md); }
  .heading, .controls, .summaries, .checking { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .heading { justify-content: space-between; }
  .controls > span { color: var(--text-muted); font-size: var(--font-size-sm); flex: 1; min-width: 200px; }
  textarea { box-sizing: border-box; width: 100%; resize: vertical; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-base); color: var(--text-primary); font-family: var(--font-mono); }
  .summaries { overflow-wrap: anywhere; }
</style>
