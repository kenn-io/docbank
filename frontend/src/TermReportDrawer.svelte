<script lang="ts">
  import { drawerPage } from "./drawerNavigation.js";
  import DrawerShell from "./DrawerShell.svelte";
  import { getContext, untrack } from "svelte";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Checkbox,
    DateRangePicker,
    IconButton,
    SegmentedControl,
    SelectDropdown,
    Spinner,
    TextInput,
    resolveRange,
  } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { collections, displayName, type Collection } from "./collections.js";
  import * as api from "./generated/docbank.js";
  import type {
    DateCandidate,
    DateChoice,
    DateReviewMember,
    Identity,
    Request,
    Summary,
    Term,
    TermReportHistory,
  } from "./generated/docbank.js";
  import { formatDate } from "./format.js";

  interface Props {
    session: string;
    initialExpression: string;
    initialDocuments?: readonly Identity[];
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, initialExpression, initialDocuments, onclose, onauthfailure }: Props = $props();

  const page = getContext<boolean>(drawerPage);
  let editing = $state(!page);

  const zone = "UTC";
  function newTerm(number: number, expression = ""): Term {
    const today = new Date().toISOString().slice(0, 10);
    return { number, expression, syntax: "simple", dates: { start: "2000-01-01", end: today } };
  }
  function newDraft(): Request {
    return {
      version: 1,
      all_documents: initialDocuments === undefined,
      selected_documents:
        initialDocuments === undefined
          ? undefined
          : { documents: initialDocuments.map((id) => ({ ...id })) },
      timezone: zone,
      coverage_mode: "strict",
      terms: [newTerm(1, initialExpression)],
    };
  }
  let draft = $state<Request>(untrack(newDraft));
  let history = $state<TermReportHistory[]>([]);
  let historyTotal = $state(0);
  let historyLoading = $state(false);
  let collectionItems = $state<Collection[]>([]);
  let collectionTotal = $state(0);
  let collectionLoading = $state(false);
  let active = $state<Summary | null>(null);
  let submittedDraft = $state<string | null>(null);
  let draftChanged = $derived(
    active !== null && submittedDraft !== null && JSON.stringify(draft) !== submittedDraft,
  );
  let review = $state<DateReviewMember[]>([]);
  let reviewCursor = $state("");
  let reviewLoaded = $state(false);
  let choices = $state<Record<string, DateChoice>>({});
  let busy = $state(false);
  let error = $state("");
  let notice = $state("");
  let optionsOpen = $state(false);
  let generation = 0;
  let historyGeneration = 0;

  $effect(() => {
    const currentSession = session;
    untrack(() => {
      void loadHistory(currentSession, true);
      void loadCollections(currentSession, true);
    });
    return () => {
      generation += 1;
      historyGeneration += 1;
    };
  });

  function fail(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return;
    }
    error =
      cause instanceof APIError && cause.code === "report_selection_changed"
        ? "Selected documents changed. Refresh the workspace and reselect the intended current versions."
        : cause instanceof Error
          ? cause.message
          : String(cause);
  }

  async function loadHistory(currentSession = session, reset = false): Promise<void> {
    if (historyLoading && !reset) return;
    const request = ++historyGeneration;
    historyLoading = true;
    const epoch = generation;
    try {
      const offset = reset ? 0 : history.length;
      const page = await api.listTermReportHistory(
        { offset, limit: 20 },
        { session: currentSession },
      );
      if (epoch !== generation || request !== historyGeneration || currentSession !== session)
        return;
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

  const syntaxOptions = [
    { value: "simple", label: "Simple" },
    { value: "advanced", label: "Advanced" },
  ];
  const coverageOptions = [
    { value: "strict", label: "Stop on incomplete coverage" },
    { value: "available_only", label: "Count available text" },
  ];
  const dateOrderOptions = [
    { value: "", label: "Review each" },
    { value: "MDY", label: "Month / day / year" },
    { value: "DMY", label: "Day / month / year" },
  ];
  const roleOptions = [
    { value: "", label: "Keep source role" },
    ...[
      "document_date",
      "created",
      "authored",
      "sent",
      "captured",
      "signed",
      "effective",
      "expiry",
    ].map((role) => ({ value: role, label: role })),
  ];
  const optionSummary = $derived(
    [
      draft.selected_documents
        ? `Selected documents (${draft.selected_documents.documents?.length ?? 0})`
        : draft.all_documents
          ? "All documents"
          : `${draft.collection_ids?.length ?? 0} import${draft.collection_ids?.length === 1 ? "" : "s"}`,
      draft.profile || "Default profile",
      draft.timezone || "UTC",
      coverageOptions.find((option) => option.value === draft.coverage_mode)?.label ?? "",
    ].join(" · "),
  );

  function addTerm(): void {
    const next = Math.max(0, ...draft.terms.map((term) => term.number)) + 1;
    draft.terms = [...draft.terms, newTerm(next)];
  }

  function openDraft(): void {
    draft = newDraft();
    editing = true;
  }

  function useHistory(item: TermReportHistory): void {
    editing = true;
    const source = item.request;
    draft = {
      ...source,
      collection_ids: [...(source.collection_ids ?? [])],
      selected_documents:
        source.selected_documents == null
          ? undefined
          : { documents: source.selected_documents.documents?.map((id) => ({ ...id })) },
      terms: source.terms.map((term) => ({ ...term, dates: { ...term.dates } })),
      date_choices: [],
    };
    active = null;
    submittedDraft = null;
    review = [];
    reviewLoaded = false;
    choices = {};
    error = "";
    notice = `Loaded the ${formatDate(item.summary.observed_at)} report.`;
  }

  async function run(): Promise<void> {
    if (busy) return;
    error = "";
    notice = "";
    if (draft.selected_documents && !draft.selected_documents.documents?.length) {
      error = "Select at least one current document in the workspace.";
      return;
    }
    if (!draft.selected_documents && !draft.all_documents && !draft.collection_ids?.length) {
      error = "Choose at least one import, or use all documents.";
      optionsOpen = true;
      return;
    }
    if (
      !draft.terms.length ||
      draft.terms.some((term) => !term.expression.trim() || !term.dates.start || !term.dates.end)
    ) {
      error = "Every term needs an expression and both cutoff dates.";
      return;
    }
    busy = true;
    const submittedFingerprint = JSON.stringify(draft);
    const currentSession = session;
    const epoch = generation;
    try {
      const request: Request = {
        ...draft,
        collection_ids: draft.all_documents ? [] : draft.collection_ids,
        terms: draft.terms.map((term) => ({ ...term, dates: { ...term.dates } })),
        date_choices: [],
      };
      const summary = await api.createTermReport(request, { session: currentSession });
      if (epoch !== generation || currentSession !== session) return;
      review = [];
      reviewCursor = "";
      reviewLoaded = false;
      choices = {};
      if (JSON.stringify(draft) !== submittedFingerprint) {
        active = null;
        submittedDraft = null;
        notice = "A report from an earlier draft finished. It's in Recent reports.";
      } else {
        active = summary;
        submittedDraft = submittedFingerprint;
        notice = "";
      }
      await loadHistory(currentSession, true);
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }

  async function loadDates(reset = false): Promise<void> {
    if (!active || busy) return;
    const id = active.id;
    const epoch = generation;
    busy = true;
    error = "";
    try {
      const page = await api.getTermReportDates(
        id,
        { cursor: reset ? "" : reviewCursor, limit: 20 },
        { session },
      );
      if (epoch !== generation || active?.id !== id) return;
      const members = new Map((reset ? [] : review).map((member) => [documentKey(member), member]));
      for (const member of page.members) {
        const key = documentKey(member);
        const existing = members.get(key);
        members.set(
          key,
          existing
            ? { ...member, candidates: [...existing.candidates, ...member.candidates] }
            : member,
        );
      }
      review = [...members.values()];
      reviewCursor = page.next_cursor ?? "";
      reviewLoaded = true;
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }

  function documentKey(member: DateReviewMember): string {
    return `${member.document.node_id}:${member.document.version_id}:${member.document.sha256}`;
  }

  function canInterpret(candidate: DateCandidate): boolean {
    return (
      candidate.rejection === "ambiguous_numeric_date" || candidate.rejection === "timezone_omitted"
    );
  }

  function displayDate(candidate: DateCandidate): string {
    return candidate.source_class === "content" && candidate.locator.rendition_id
      ? candidate.raw.replace(/\\([-/,])/g, "$1")
      : candidate.raw;
  }

  function choose(member: DateReviewMember, candidate: DateCandidate): void {
    const key = documentKey(member);
    choices = {
      ...choices,
      [key]: {
        action: canInterpret(candidate) ? "interpret" : "select",
        candidate_id: candidate.id,
        document: member.document,
        evidence_sha256: candidate.locator.evidence_sha256 ?? "",
        reason: "",
      },
    };
  }

  function updateChoice(member: DateReviewMember, field: keyof DateChoice, value: string): void {
    const key = documentKey(member);
    const current = choices[key];
    if (current)
      choices = {
        ...choices,
        [key]:
          field === "action"
            ? {
                ...current,
                action: value,
                reviewed_date: undefined,
                reviewed_timezone: undefined,
                reviewed_role: undefined,
              }
            : { ...current, [field]: value },
      };
  }

  async function revise(): Promise<void> {
    if (!active || busy) return;
    const id = active.id;
    const epoch = generation;
    const selected = Object.values(choices);
    if (!selected.length || selected.some((choice) => !choice.reason.trim())) {
      error = "Choose evidence and give a reason for each reviewed date.";
      return;
    }
    if (
      selected.some(
        (choice) =>
          choice.action === "interpret" &&
          (!choice.reviewed_date?.trim() || !choice.reviewed_timezone?.trim()),
      )
    ) {
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
      notice = "";
      await loadHistory(session, true);
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
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
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }
</script>

{#snippet reportBody()}
  {#snippet reportForm()}
    <section class="tool-section" aria-label="Report terms">
      <div class="terms" role="table" aria-label="Terms">
        <div class="term-row term-head" role="row">
          <span role="columnheader">Expression</span><span role="columnheader">Syntax</span><span
            role="columnheader">Date range</span
          ><span role="columnheader"><span class="kit-sr-only">Remove</span></span>
        </div>
        {#each draft.terms as term, index (term.number)}
          <div class="term-row" role="row">
            <span role="cell"
              ><TextInput
                ariaLabel={`Expression for term ${term.number}`}
                bind:value={term.expression}
                placeholder="Search expression"
                autofocus={index === 0}
                block
              /></span
            >
            <span role="cell"
              ><SelectDropdown
                title={`Syntax for term ${term.number}`}
                value={term.syntax}
                options={syntaxOptions}
                onchange={(value) => (term.syntax = value)}
              /></span
            >
            <span role="cell"
              ><DateRangePicker
                selection={{ mode: "custom", from: term.dates.start, to: term.dates.end }}
                presets={[]}
                align="right"
                onSelect={(selection) => {
                  const range = resolveRange(selection);
                  term.dates = { start: range.from, end: range.to };
                }}
              /></span
            >
            <span role="cell"
              >{#if draft.terms.length > 1}<IconButton
                  size="sm"
                  ariaLabel={`Remove term ${term.number}`}
                  title="Remove term"
                  onclick={() => (draft.terms = draft.terms.filter((_, at) => at !== index))}
                  ><XIcon size="14" aria-hidden="true" /></IconButton
                >{/if}</span
            >
          </div>
        {/each}
      </div>
      <div class="tool-actions tool-actions--start">
        <Button size="sm" disabled={draft.terms.length >= 128} onclick={addTerm}
          ><PlusIcon size="14" aria-hidden="true" /> Add term</Button
        >
      </div>
    </section>

    <details class="tool-disclosure" bind:open={optionsOpen}>
      <summary
        ><ChevronRightIcon size="14" aria-hidden="true" /><span class="tool-label">Options</span
        ><span class="tool-disclosure-value">{optionSummary}</span></summary
      >
      <div class="tool-grid">
        <div class="tool-field tool-span-all">
          <span class="tool-label">Sources</span>
          {#if draft.selected_documents}
            <span>Selected documents ({draft.selected_documents.documents?.length ?? 0})</span>
          {:else}
            <div class="scope">
              <SegmentedControl
                ariaLabel="Sources"
                value={draft.all_documents ? "all" : "imports"}
                options={[
                  { value: "all", label: "All documents" },
                  { value: "imports", label: "Selected imports" },
                ]}
                onchange={(value) => (draft.all_documents = value === "all")}
              />
            </div>
            {#if !draft.all_documents}
              <div class="collection-list">
                {#each collectionItems as collection (collection.id)}
                  <Checkbox
                    checked={(draft.collection_ids ?? []).includes(collection.id)}
                    onchange={() => toggleCollection(collection.id)}
                    label={`${displayName(collection)} · ${collection.file_count} files`}
                  />
                {/each}
                {#if !collectionItems.length && collectionLoading}<span
                    ><Spinner size={14} /> Loading…</span
                  >{/if}
                {#if !collectionItems.length && !collectionLoading}<span class="tool-muted"
                    >No imports yet.</span
                  >{/if}
                {#if collectionItems.length < collectionTotal}<div
                    class="tool-actions tool-actions--start"
                  >
                    <Button
                      size="sm"
                      disabled={collectionLoading}
                      onclick={() => void loadCollections()}
                      >{collectionLoading ? "Loading…" : "Show more"}</Button
                    >
                  </div>{/if}
              </div>
            {/if}
          {/if}
        </div>
        <div class="tool-field tool-span-3">
          <label for="report-profile">Processing profile</label>
          <TextInput
            id="report-profile"
            value={draft.profile ?? ""}
            oninput={(value) => (draft.profile = value)}
            placeholder="Default"
            block
          />
        </div>
        <div class="tool-field tool-span-3">
          <span class="tool-label" id="report-coverage-label">Coverage</span>
          <SelectDropdown
            title="Coverage policy"
            value={draft.coverage_mode}
            options={coverageOptions}
            onchange={(value) => (draft.coverage_mode = value)}
          />
        </div>
        <div class="tool-field tool-span-2">
          <label for="report-zone">Report timezone</label>
          <TextInput id="report-zone" bind:value={draft.timezone} placeholder="UTC" block />
        </div>
        <div class="tool-field tool-span-2">
          <label for="report-source-zone">Fallback source timezone</label>
          <TextInput
            id="report-source-zone"
            value={draft.source_timezone ?? ""}
            oninput={(value) => (draft.source_timezone = value)}
            placeholder="None"
            block
          />
        </div>
        <div class="tool-field tool-span-2">
          <span class="tool-label">Ambiguous dates</span>
          <SelectDropdown
            title="Ambiguous numeric dates"
            value={draft.numeric_date_order ?? ""}
            options={dateOrderOptions}
            onchange={(value) => (draft.numeric_date_order = value)}
          />
        </div>
      </div>
    </details>

    <div class="tool-actions">
      {#if page}<Button disabled={busy} onclick={() => (editing = false)}>Cancel</Button>{/if}
      <Button tone="info" surface="solid" disabled={busy} onclick={() => void run()}
        >{busy ? "Creating report…" : "Create report"}</Button
      >
    </div>
  {/snippet}
  <div class="tool-body">
    {#if error}<p class="tool-error" role="alert">{error}</p>{/if}
    {#if notice}<p role="status">{notice}</p>{/if}

    {#if editing}{@render reportForm()}{/if}

    {#if active}
      <Card level="default" padding="sm" title={draftChanged ? "Previous report" : "Report"}>
        {#snippet actions()}<span class="tool-muted"
            >{active?.state === "complete" ? "Counts ready" : "Dates need review"} · {formatDate(
              active?.observed_at ?? "",
            )}</span
          >{/snippet}
        <div class="result">
          {#if draftChanged}<p role="status">These counts use your previous search.</p>{/if}
          {#if active.state === "needs_review"}<p>
              {active.unresolved_dates} document date{active.unresolved_dates === 1 ? "" : "s"} need review
              before counts are shown.
            </p>
          {:else}<p class="tool-muted">
              {active.coverage.scoped} scoped · {active.coverage.searchable} searchable · {active
                .coverage.missing_text} missing text · {active.coverage.incomplete_families} incomplete
              families · {active.coverage.fallback_dates} fallback dates · expires {formatDate(
                active.expires_at,
              )}
            </p>{/if}
          {#if active.coverage.warnings?.length}<ul>
              {#each active.coverage.warnings as warning}<li>{warning}</li>{/each}
            </ul>{/if}
          {#if active.state === "complete" && active.counts}
            <div class="count-table" role="table" aria-label="Search report counts">
              <div class="count-row header" role="row">
                <span role="columnheader">#</span><span role="columnheader">Terms</span><span
                  role="columnheader">Date range</span
                ><span role="columnheader">Hits</span><span role="columnheader"
                  >Hits plus family</span
                ><span role="columnheader">Unique hits</span><span role="columnheader"
                  >Documents in unique families</span
                ><span role="columnheader">Unique hits plus family</span>
              </div>
              {#each active.counts as count, index}<div class="count-row" role="row">
                  <span role="cell">{active.terms[index]?.number}</span><span role="cell"
                    >{active.terms[index]?.expression}</span
                  ><span role="cell"
                    >{active.terms[index]?.dates.start} to {active.terms[index]?.dates.end}</span
                  ><span role="cell">{count.hits}</span><span role="cell"
                    >{count.hits_plus_family}</span
                  ><span role="cell">{count.unique_hits}</span><span role="cell"
                    >{count.unique_families}</span
                  ><span role="cell">{count.unique_hits_plus_family}</span>
                </div>{/each}
            </div>
          {/if}
          <div class="tool-actions">
            <Button
              disabled={busy}
              tone={active.state === "complete" ? "neutral" : "info"}
              surface={active.state === "complete" ? "outline" : "solid"}
              onclick={() => void loadDates(true)}>Review dates</Button
            >
            {#if active.state === "complete" && active.counts}
              <Button disabled={busy} onclick={() => void download("bundle")}
                >{draftChanged ? "Download previous evidence ZIP" : "Download evidence ZIP"}</Button
              ><Button
                tone="info"
                surface="solid"
                disabled={busy}
                onclick={() => void download("csv")}
                >{draftChanged ? "Download previous CSV" : "Download CSV"}</Button
              >
            {/if}
          </div>
          {#if reviewLoaded}
            <div class="review">
              {#each review as member (documentKey(member))}
                <div
                  class="review-item"
                  role="group"
                  aria-label={`Document ${member.document.node_id}`}
                >
                  <div class="review-title">
                    <strong>Document {member.document.node_id}</strong>
                    <span class="tool-muted"
                      >{member.selection.date
                        ? `Selected ${member.selection.date} (${member.selection.reason})`
                        : "Date unresolved"}</span
                    >
                  </div>
                  {#each member.candidates as candidate (candidate.id)}
                    <label class="candidate"
                      ><input
                        type="radio"
                        name={`review-${documentKey(member)}`}
                        disabled={!!candidate.rejection && !canInterpret(candidate)}
                        checked={choices[documentKey(member)]?.candidate_id === candidate.id}
                        onchange={() => choose(member, candidate)}
                      />
                      <span
                        >{candidate.role} · {displayDate(candidate)} · {candidate.source_class}{candidate.rejection
                          ? ` · ${candidate.rejection}`
                          : ""}</span
                      ></label
                    >
                  {/each}
                  {#if choices[documentKey(member)]}
                    {@const choice = choices[documentKey(member)]}
                    {@const candidate = member.candidates.find(
                      (item) => item.id === choice.candidate_id,
                    )}
                    <div class="tool-grid">
                      <div class="tool-field tool-span-3">
                        <span class="tool-label">Action</span>
                        <SelectDropdown
                          title="Action"
                          value={choice.action}
                          options={candidate && canInterpret(candidate)
                            ? [{ value: "interpret", label: "Interpret source date" }]
                            : [
                                { value: "select", label: "Select source date" },
                                { value: "reclassify", label: "Reclassify role" },
                              ]}
                          onchange={(value) => updateChoice(member, "action", value)}
                        />
                      </div>
                      <div class="tool-field tool-span-3">
                        <span class="tool-label">Reason</span>
                        <TextInput
                          ariaLabel="Reason"
                          value={choice.reason}
                          oninput={(value) => updateChoice(member, "reason", value)}
                          block
                        />
                      </div>
                      {#if choice.action === "interpret"}
                        <div class="tool-field tool-span-3">
                          <span class="tool-label">Reviewed date</span>
                          <TextInput
                            ariaLabel="Reviewed date"
                            value={choice.reviewed_date ?? ""}
                            oninput={(value) => updateChoice(member, "reviewed_date", value)}
                            placeholder="YYYY-MM-DD"
                            block
                          />
                        </div>
                        <div class="tool-field tool-span-3">
                          <span class="tool-label">Source timezone</span>
                          <TextInput
                            ariaLabel="Source timezone"
                            value={choice.reviewed_timezone ?? ""}
                            oninput={(value) => updateChoice(member, "reviewed_timezone", value)}
                            placeholder="UTC"
                            block
                          />
                        </div>
                      {/if}
                      {#if choice.action !== "select"}
                        <div class="tool-field tool-span-3">
                          <span class="tool-label">Reviewed role</span>
                          <SelectDropdown
                            title="Reviewed role"
                            value={choice.reviewed_role ?? ""}
                            options={roleOptions}
                            onchange={(value) => updateChoice(member, "reviewed_role", value)}
                          />
                        </div>
                      {/if}
                    </div>
                  {/if}
                </div>
              {/each}
              <div class="tool-actions">
                {#if reviewCursor}<Button disabled={busy} onclick={() => void loadDates()}
                    >More evidence</Button
                  >{/if}
                <Button
                  tone="info"
                  surface="solid"
                  disabled={busy || !Object.keys(choices).length}
                  onclick={() => void revise()}>Create reviewed revision</Button
                >
              </div>
            </div>
          {/if}
        </div>
      </Card>
    {/if}

    <section class="tool-section" aria-labelledby="report-history-heading">
      <h3 id="report-history-heading">Recent reports</h3>
      {#if !history.length && historyLoading}<p class="tool-muted">
          <Spinner size={14} /> Loading…
        </p>{/if}
      {#if !history.length && !historyLoading}<p class="tool-muted">No reports yet.</p>{/if}
      {#if history.length}
        <ul class="tool-list">
          {#each history as item (item.summary.id)}
            <li>
              <div class="tool-row-main">
                <strong>{item.request.terms.map((term) => term.expression).join(" · ")}</strong>
                <span
                  >{formatDate(item.summary.observed_at)} · {item.summary.state === "complete"
                    ? "Counts ready"
                    : "Dates need review"} · {item.request.selected_documents
                    ? `Selected documents (${item.request.selected_documents.documents?.length ?? 0})`
                    : item.request.all_documents
                      ? "All documents"
                      : `${item.request.collection_ids?.length ?? 0} import${item.request.collection_ids?.length === 1 ? "" : "s"}`}</span
                >
              </div>
              <Button size="sm" onclick={() => useHistory(item)}>Use as draft</Button>
            </li>
          {/each}
        </ul>
      {/if}
      {#if history.length < historyTotal}<div class="tool-actions tool-actions--start">
          <Button size="sm" disabled={historyLoading} onclick={() => void loadHistory()}
            >{historyLoading ? "Loading…" : "Show more"}</Button
          >
        </div>{/if}
    </section>
  </div>
{/snippet}

<DrawerShell
  title={page ? "Reports" : "Search term report"}
  level={page ? 2 : 1}
  closeLabel="Close search term report"
  {onclose}
>
  {#snippet actions()}
    {#if page && !editing}<Button size="sm" onclick={openDraft}>Run report</Button>{/if}
  {/snippet}
  {@render reportBody()}
</DrawerShell>

<style>
  .terms {
    display: grid;
    gap: var(--space-3);
  }
  .term-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 120px 200px 28px;
    align-items: center;
    gap: var(--space-4);
  }
  .term-row > span {
    min-width: 0;
  }
  .term-head {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium, 500);
  }
  .term-row :global(.kit-select-dropdown),
  .term-row :global(.kit-date-range-picker__trigger) {
    width: 100%;
    min-width: 0;
  }
  .scope {
    display: flex;
  }
  .collection-list {
    display: grid;
    gap: var(--space-3);
    max-height: 220px;
    overflow: auto;
    padding: var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  .result,
  .review {
    display: grid;
    gap: var(--space-5);
  }
  .result ul {
    margin: 0;
    padding-left: var(--space-7);
  }
  .review-item {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-5);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  .review-title {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-4);
  }
  .candidate {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }
  .count-table {
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  .count-row {
    display: grid;
    grid-template-columns: 32px minmax(140px, 2fr) 190px repeat(5, minmax(84px, 1fr));
    min-width: 900px;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-muted);
    font-variant-numeric: tabular-nums;
  }
  .count-row:last-child {
    border-bottom: 0;
  }
  .count-row.header {
    align-items: end;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium, 500);
    background: var(--bg-inset);
  }
  @media (max-width: 640px) {
    .term-row {
      grid-template-columns: minmax(0, 1fr) 28px;
    }
    .term-head {
      display: none;
    }
    .term-row > span:first-child {
      grid-column: 1 / -1;
    }
  }
</style>
