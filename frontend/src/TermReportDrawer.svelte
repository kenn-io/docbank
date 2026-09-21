<script lang="ts">
  import XIcon from "@lucide/svelte/icons/x";
  import { untrack } from "svelte";
  import { Button, Card, Checkbox, DateRangePicker, DetailDrawer, IconButton, Spinner, TextInput, resolveRange } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { collections, type Collection } from "./collections.js";
  import * as api from "./generated/docbank.js";
  import type { DateCandidate, DateChoice, DateReviewMember, Request, Summary, Term, TermReportHistory } from "./generated/docbank.js";
  import { formatDate } from "./format.js";

  interface Props {
    session: string;
    initialExpression: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, initialExpression, onclose, onauthfailure }: Props = $props();

  const today = new Date().toISOString().slice(0, 10);
  const zone = "UTC";
  function newTerm(number: number, expression = ""): Term {
    return { number, expression, syntax: "simple", dates: { start: "2000-01-01", end: today } };
  }
  let draft = $state<Request>({ version: 1, all_documents: true, timezone: zone,
    coverage_mode: "strict", terms: [newTerm(1, untrack(() => initialExpression))] });
  let history = $state<TermReportHistory[]>([]);
  let historyTotal = $state(0);
  let historyLoading = $state(false);
  let collectionItems = $state<Collection[]>([]);
  let collectionTotal = $state(0);
  let collectionLoading = $state(false);
  let active = $state<Summary | null>(null);
  let submittedDraft = $state<string | null>(null);
  let draftChanged = $derived(active !== null && submittedDraft !== null && JSON.stringify(draft) !== submittedDraft);
  let review = $state<DateReviewMember[]>([]);
  let reviewCursor = $state("");
  let reviewLoaded = $state(false);
  let choices = $state<Record<string, DateChoice>>({});
  let busy = $state(false);
  let error = $state("");
  let notice = $state("");
  let generation = 0;
  let historyGeneration = 0;

  $effect(() => {
    const currentSession = session;
    untrack(() => {
      void loadHistory(currentSession, true);
      void loadCollections(currentSession, true);
    });
    return () => { generation += 1; historyGeneration += 1; };
  });

  function fail(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function loadHistory(currentSession = session, reset = false): Promise<void> {
    if (historyLoading && !reset) return;
    const request = ++historyGeneration;
    historyLoading = true;
    const epoch = generation;
    try {
      const offset = reset ? 0 : history.length;
      const page = await api.listTermReportHistory({ offset, limit: 20 }, { session: currentSession });
      if (epoch !== generation || request !== historyGeneration || currentSession !== session) return;
      history = reset ? page.items : [...history, ...page.items];
      historyTotal = page.total;
    } catch (cause) {
      if (epoch === generation && request === historyGeneration) fail(cause);
    } finally {
      if (epoch === generation && request === historyGeneration) historyLoading = false;
    }
  }

  async function loadCollections(currentSession = session, reset = false): Promise<void> {
    if (collectionLoading) return;
    collectionLoading = true;
    const epoch = generation;
    try {
      const offset = reset ? 0 : collectionItems.length;
      const page = await collections(currentSession, offset, 100);
      if (epoch !== generation || currentSession !== session) return;
      collectionItems = reset ? page.items : [...collectionItems, ...page.items];
      collectionTotal = page.total;
    } catch (cause) {
      if (epoch === generation) fail(cause);
    } finally {
      if (epoch === generation) collectionLoading = false;
    }
  }

  function toggleCollection(id: string): void {
    const selected = new Set(draft.collection_ids ?? []);
    if (selected.has(id)) selected.delete(id);
    else selected.add(id);
    draft.collection_ids = [...selected];
  }

  function addTerm(): void {
    const next = Math.max(0, ...draft.terms.map(term => term.number)) + 1;
    draft.terms = [...draft.terms, newTerm(next)];
  }

  function useHistory(item: TermReportHistory): void {
    const source = item.request;
    draft = { ...source, collection_ids: [...(source.collection_ids ?? [])],
      terms: source.terms.map(term => ({ ...term, dates: { ...term.dates } })), date_choices: [] };
    active = null;
    submittedDraft = null;
    review = [];
    reviewLoaded = false;
    choices = {};
    error = "";
    notice = `Loaded the ${formatDate(item.summary.observed_at)} request. Edit its scope or dates, then run it on current data.`;
  }

  async function run(): Promise<void> {
    if (busy) return;
    error = "";
    notice = "";
    if (!draft.all_documents && !(draft.collection_ids?.length)) {
      error = "Select at least one collection, or use all documents.";
      return;
    }
    if (!draft.terms.length || draft.terms.some(term => !term.expression.trim() || !term.dates.start || !term.dates.end)) {
      error = "Every term needs an expression and both cutoff dates.";
      return;
    }
    busy = true;
    const submittedFingerprint = JSON.stringify(draft);
    const currentSession = session;
    const epoch = generation;
    try {
      const request: Request = { ...draft, collection_ids: draft.all_documents ? [] : draft.collection_ids,
        terms: draft.terms.map(term => ({ ...term, dates: { ...term.dates } })), date_choices: [] };
      const summary = await api.createTermReport(request, { session: currentSession });
      if (epoch !== generation || currentSession !== session) return;
      review = [];
      reviewCursor = "";
      reviewLoaded = false;
      choices = {};
      if (JSON.stringify(draft) !== submittedFingerprint) {
        active = null;
        submittedDraft = null;
        notice = "An export finished from an earlier draft. Find it in recent exports to inspect or rerun it.";
      } else {
        active = summary;
        submittedDraft = submittedFingerprint;
        notice = summary.state === "needs_review" ? "The observation is frozen. Review the unresolved dates to produce counts." :
          "Counts are ready for this frozen observation. Rerunning from history makes a new observation.";
      }
      await loadHistory(currentSession, true);
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }

  async function loadDates(reset = false): Promise<void> {
    if (!active || busy) return;
    const id = active.id;
    const epoch = generation;
    busy = true;
    error = "";
    try {
      const page = await api.getTermReportDates(id,
        { cursor: reset ? "" : reviewCursor, limit: 20 }, { session });
      if (epoch !== generation || active?.id !== id) return;
      review = reset ? page.members : [...review, ...page.members];
      reviewCursor = page.next_cursor ?? "";
      reviewLoaded = true;
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }

  function documentKey(member: DateReviewMember): string {
    return `${member.document.node_id}:${member.document.version_id}:${member.document.sha256}`;
  }

  function canInterpret(candidate: DateCandidate): boolean {
    return candidate.rejection === "ambiguous_numeric_date" || candidate.rejection === "timezone_omitted";
  }

  function choose(member: DateReviewMember, candidate: DateCandidate): void {
    const key = documentKey(member);
    choices = { ...choices, [key]: { action: canInterpret(candidate) ? "interpret" : "select",
      candidate_id: candidate.id, document: member.document,
      evidence_sha256: candidate.locator.evidence_sha256 ?? "", reason: "" } };
  }

  function updateChoice(member: DateReviewMember, field: keyof DateChoice, value: string): void {
    const key = documentKey(member);
    const current = choices[key];
    if (current) choices = { ...choices, [key]: field === "action"
      ? { ...current, action: value, reviewed_date: undefined, reviewed_timezone: undefined, reviewed_role: undefined }
      : { ...current, [field]: value } };
  }

  async function revise(): Promise<void> {
    if (!active || busy) return;
    const id = active.id;
    const epoch = generation;
    const selected = Object.values(choices);
    if (!selected.length || selected.some(choice => !choice.reason.trim())) {
      error = "Choose evidence and give a reason for each reviewed date.";
      return;
    }
    if (selected.some(choice => choice.action === "interpret" &&
      (!choice.reviewed_date?.trim() || !choice.reviewed_timezone?.trim()))) {
      error = "Enter a reviewed date and source timezone for each interpreted date.";
      return;
    }
    busy = true;
    error = "";
    try {
      const revised = await api.reviseTermReport(id, { choices: selected }, { session });
      if (epoch !== generation || active?.id !== id) return;
      active = revised;
      review = [];
      reviewLoaded = false;
      choices = {};
      notice = active.state === "complete" ? "Reviewed counts are ready in a new frozen revision." :
        "This revision still has unresolved dates. Continue reviewing its evidence.";
      await loadHistory(session, true);
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }

  async function download(format: "csv" | "bundle"): Promise<void> {
    if (!active || busy) return;
    busy = true;
    error = "";
    try {
      const ticket = await api.issueTermReportDownload(active.id, { format }, { session });
      const link = document.createElement("a");
      link.href = ticket.url;
      document.body.append(link);
      link.click();
      link.remove();
      notice = "Download handed to the browser. Check its download list for completion.";
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }
</script>

<DetailDrawer width="min(900px, 100vw)" ariaLabel="Search exports" onclose={onclose}>
  {#snippet header()}
    <div class="heading"><div><span>SEARCH EXPORTS</span><strong>Export search counts</strong></div>
      <IconButton size="sm" ariaLabel="Close search exports" onclick={onclose}><XIcon size="16" aria-hidden="true" /></IconButton></div>
  {/snippet}
  <div class="body">
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    {#if notice}<p role="status">{notice}</p>{/if}
    <Card level="default" padding="sm" title="New search export">
      <div class="form">
        <div class="field"><label for="report-profile">Processing profile</label><input id="report-profile" bind:value={draft.profile} placeholder="Configured default" /></div>
        <div class="field"><label for="report-zone">Export timezone</label><input id="report-zone" bind:value={draft.timezone} placeholder="UTC" /></div>
        <div class="field"><label for="report-source-zone">Source timezone when omitted</label><input id="report-source-zone" bind:value={draft.source_timezone} placeholder="Optional" /></div>
        <div class="field"><label for="report-date-order">Ambiguous numeric dates</label><select id="report-date-order" bind:value={draft.numeric_date_order}>
          <option value="">Review each</option><option value="MDY">Month / day / year</option><option value="DMY">Day / month / year</option></select></div>
        <div class="field"><label for="report-coverage">Coverage policy</label><select id="report-coverage" bind:value={draft.coverage_mode}>
          <option value="strict">Strict — stop on incomplete coverage</option><option value="available_only">Available text only — show gaps</option></select></div>
      </div>
      <section class="scope" aria-label="Export source scope">
        <strong>Sources</strong>
        <label><input type="radio" name="report-scope" checked={draft.all_documents} onchange={() => draft.all_documents = true} /> All documents</label>
        <label><input type="radio" name="report-scope" checked={!draft.all_documents} onchange={() => draft.all_documents = false} /> Selected collections</label>
        {#if !draft.all_documents}
          <div class="collection-list">
            {#each collectionItems as collection (collection.id)}
              <Checkbox checked={(draft.collection_ids ?? []).includes(collection.id)} onchange={() => toggleCollection(collection.id)}
                label={`${collection.label || collection.source_description} · ${collection.file_count} files`} />
            {/each}
            {#if collectionItems.length < collectionTotal}<Button size="sm" disabled={collectionLoading} onclick={() => void loadCollections()}>{collectionLoading ? "Loading…" : "More collections"}</Button>{/if}
            {#if !collectionItems.length && collectionLoading}<span><Spinner size={14} /> Loading collections…</span>{/if}
          </div>
        {/if}
      </section>
      <section class="terms" aria-label="Export search queries">
        <div class="section-heading"><strong>Terms and cutoff dates</strong><Button size="sm" disabled={draft.terms.length >= 128} onclick={addTerm}>Add term</Button></div>
        {#each draft.terms as term, index (term.number)}
          <div class="term"><span>Term {term.number}</span><select aria-label={`Syntax for term ${term.number}`} bind:value={term.syntax}>
              <option value="simple">Simple</option><option value="advanced">Advanced</option></select>
            <input aria-label={`Expression for term ${term.number}`} bind:value={term.expression} placeholder="Search expression" />
            <div class="date-picker"><span>Inclusive date range</span><DateRangePicker block selection={{ mode: "custom", from: term.dates.start, to: term.dates.end }}
              presets={[]} onSelect={selection => { const range = resolveRange(selection); term.dates = { start: range.from, end: range.to }; }} /></div>
            <Button size="sm" disabled={draft.terms.length === 1} onclick={() => draft.terms = draft.terms.filter((_, at) => at !== index)}>Remove</Button></div>
        {/each}
      </section>
      <Button tone="info" disabled={busy} onclick={() => void run()}>{busy ? "Preparing export…" : "Create export"}</Button>
    </Card>

    {#if active}
      <Card level="default" padding="sm" title={draftChanged ? "Previous export" : "Current export"}>
        <div class="result"><p><strong>{active.state === "complete" ? "Counts ready" : "Date review needed"}</strong> · observed {formatDate(active.observed_at)} · handle expires {formatDate(active.expires_at)}</p>
          {#if draftChanged}<p role="status">Draft changed; the counts and downloads below belong to the previous frozen run. Create an export to use the edited search.</p>{/if}
          {#if active.state === "needs_review"}<p>Coverage pending date review; counts are withheld.</p>
          {:else}<p>{active.coverage.scoped} scoped · {active.coverage.searchable} searchable · {active.coverage.missing_text} missing text · {active.coverage.incomplete_families} incomplete families · {active.coverage.fallback_dates} fallback dates</p>{/if}
          {#if active.coverage.warnings?.length}<ul>{#each active.coverage.warnings as warning}<li>{warning}</li>{/each}</ul>{/if}
          {#if active.state === "complete" && active.counts}
            <div class="count-table" role="table" aria-label="Search export counts">
              <div class="count-row header" role="row"><span>Term #</span><span>Terms</span><span>Date Range</span><span>Hits</span><span>Hits Plus Family</span><span>Unique Hits</span><span>Unique Families</span><span>Unique Hits Plus Family</span></div>
              {#each active.counts as count, index}<div class="count-row" role="row"><span>{active.terms[index]?.number}</span><span>{active.terms[index]?.expression}</span><span>{active.terms[index]?.dates.start} to {active.terms[index]?.dates.end}</span><span>{count.hits}</span><span>{count.hits_plus_family}</span><span>{count.unique_hits}</span><span>{count.unique_families}</span><span>{count.unique_hits_plus_family}</span></div>{/each}
            </div>
            <div class="actions"><Button disabled={busy} onclick={() => void download("csv")}>{draftChanged ? "Download previous CSV" : "Download CSV"}</Button><Button disabled={busy} onclick={() => void download("bundle")}>{draftChanged ? "Download previous evidence ZIP" : "Download evidence ZIP"}</Button></div>
          {:else}
            <p>{active.unresolved_dates} document dates need review. Counts are withheld until they are resolved.</p>
          {/if}
          <Button disabled={busy} onclick={() => void loadDates(true)}>Review dates</Button>
          {#if reviewLoaded}
              <div class="review">
                {#each review as member (documentKey(member))}
                  <div class="review-item"><strong>Document {member.document.node_id}</strong>
                    {#if member.selection.date}<span>Selected {member.selection.date} ({member.selection.reason})</span>{:else}<span>Date unresolved</span>{/if}
                    {#each member.candidates as candidate (candidate.id)}
                      <label class="candidate"><input type="radio" name={`review-${documentKey(member)}`} disabled={!!candidate.rejection && !canInterpret(candidate)} checked={choices[documentKey(member)]?.candidate_id === candidate.id} onchange={() => choose(member, candidate)} />
                        <span>{candidate.role} · {candidate.raw} · {candidate.source_class}{candidate.rejection ? ` · ${candidate.rejection}` : ""}</span></label>
                    {/each}
                    {#if choices[documentKey(member)]}
                      {@const candidate = member.candidates.find(item => item.id === choices[documentKey(member)].candidate_id)}
                      <div class="review-form"><label>Action <select value={choices[documentKey(member)].action} onchange={event => updateChoice(member, "action", event.currentTarget.value)}>
                        {#if candidate && canInterpret(candidate)}<option value="interpret">Interpret source date</option>
                        {:else}<option value="select">Select source date</option><option value="reclassify">Reclassify role</option>{/if}
                      </select></label>
                        <label>Reason <input value={choices[documentKey(member)].reason} oninput={event => updateChoice(member, "reason", event.currentTarget.value)} /></label>
                        {#if choices[documentKey(member)].action === "interpret"}<label>Reviewed date (YYYY-MM-DD) <TextInput value={choices[documentKey(member)].reviewed_date ?? ""} oninput={value => updateChoice(member, "reviewed_date", value)} block /></label>
                          <label>Source timezone <input value={choices[documentKey(member)].reviewed_timezone ?? ""} oninput={event => updateChoice(member, "reviewed_timezone", event.currentTarget.value)} placeholder="UTC" /></label>{/if}
                        {#if choices[documentKey(member)].action !== "select"}<label>Reviewed role <select value={choices[documentKey(member)].reviewed_role ?? ""} onchange={event => updateChoice(member, "reviewed_role", event.currentTarget.value)}><option value="">Keep source role</option>{#each ["document_date", "created", "authored", "sent", "captured", "signed", "effective", "expiry"] as role}<option value={role}>{role}</option>{/each}</select></label>{/if}</div>
                    {/if}
                  </div>
                {/each}
                <div class="actions">{#if reviewCursor}<Button disabled={busy} onclick={() => void loadDates()}>More evidence</Button>{/if}
                  <Button tone="info" disabled={busy || !Object.keys(choices).length} onclick={() => void revise()}>Create reviewed revision</Button></div>
              </div>
          {/if}
        </div>
      </Card>
    {/if}

    <Card level="default" padding="sm" title="Recent exports">
      <div class="history"><p>The latest 100 export requests and outcomes stay in this vault. Download links and reviewed evidence expire; using a request as a draft starts a fresh run.</p>
        {#if !history.length && historyLoading}<p><Spinner size={14} /> Loading history…</p>{/if}
        {#if !history.length && !historyLoading}<p>No searches have been exported yet.</p>{/if}
        {#each history as item (item.summary.id)}
          <div class="history-item"><div><strong>{formatDate(item.summary.observed_at)}</strong><span>{item.summary.state === "complete" ? "Counts ready" : "Date review needed"} · {item.request.terms.length} term{item.request.terms.length === 1 ? "" : "s"} · {item.request.all_documents ? "All documents" : `${item.request.collection_ids?.length ?? 0} collection${item.request.collection_ids?.length === 1 ? "" : "s"}`}</span>
              <small>{item.request.terms.map(term => term.expression).join(" · ")}</small></div><Button size="sm" onclick={() => useHistory(item)}>Use as draft</Button></div>
        {/each}
        {#if history.length < historyTotal}<Button size="sm" disabled={historyLoading} onclick={() => void loadHistory()}>{historyLoading ? "Loading…" : "Older runs"}</Button>{/if}
      </div>
    </Card>
  </div>
</DetailDrawer>

<style>
  .heading, .section-heading, .history-item, .actions { display:flex; justify-content:space-between; align-items:center; gap:var(--space-3); }
  .heading > div, .body, .form, .scope, .terms, .result, .review, .history, .review-item, .review-form { display:grid; gap:var(--space-3); }
  .heading span { font-size:var(--font-size-xs); color:var(--text-muted); font-weight:var(--font-weight-bold); }
  .heading strong { font-size:var(--font-size-lg); }
  .body { padding:var(--space-5); gap:var(--space-5); }
  .form { grid-template-columns:repeat(2,minmax(0,1fr)); }
  .field { display:grid; gap:var(--space-1); }
  .field label, .review-form label { font-size:var(--font-size-sm); }
  input:not([type="checkbox"]):not([type="radio"]), select { width:100%; min-width:0; padding:var(--space-2); border:1px solid var(--border-muted); border-radius:var(--radius-md); background:var(--bg-base); color:var(--text-primary); font:inherit; }
  .scope label, .candidate { display:flex; align-items:center; gap:var(--space-2); font-size:var(--font-size-sm); }
  .collection-list { display:grid; gap:var(--space-2); max-height:220px; overflow:auto; padding:var(--space-2); border:1px solid var(--border-muted); border-radius:var(--radius-md); }
  .term, .review-item, .history-item { padding:var(--space-3); border:1px solid var(--border-muted); border-radius:var(--radius-md); background:var(--bg-inset); }
  .term { display:grid; grid-template-columns:auto 110px minmax(120px,1fr) 240px auto; gap:var(--space-2); align-items:end; }
  .date-picker { display:grid; gap:var(--space-1); font-size:var(--font-size-sm); }
  .review-form { grid-template-columns:repeat(2,minmax(0,1fr)); }
  .review-form label { display:grid; gap:var(--space-1); }
  .history-item > div { display:grid; gap:var(--space-1); min-width:0; }
  .history-item span, .history-item small, p { font-size:var(--font-size-sm); color:var(--text-secondary); }
  .history-item small { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .count-table { overflow:auto; border:1px solid var(--border-muted); border-radius:var(--radius-md); }
  .count-row { display:grid; grid-template-columns:70px minmax(180px,2fr) minmax(190px,1.5fr) repeat(5,minmax(100px,1fr)); min-width:1080px; gap:var(--space-2); padding:var(--space-2); font-size:var(--font-size-sm); border-bottom:1px solid var(--border-muted); }
  .count-row.header { font-weight:var(--font-weight-bold); background:var(--bg-inset); }
  .actions { justify-content:flex-start; flex-wrap:wrap; }
  p { margin:0; }
  .error { color:var(--accent-red); }
  @media(max-width:760px) { .form, .review-form { grid-template-columns:1fr; } .term { grid-template-columns:1fr 1fr; } .term input[aria-label^="Expression"] { grid-column:1 / -1; } }
</style>
