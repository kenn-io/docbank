<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import HardDriveIcon from "@lucide/svelte/icons/hard-drive";
  import SendIcon from "@lucide/svelte/icons/send";
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import { Button, Checkbox, Modal, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { startProcessing } from "./receipts.js";
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";
  import {
    consentLabel,
    externalDestinations,
    describeClasses,
    formatBytes,
    ineligibleSentence,
    keptSentence,
    processingPurpose,
    processingSetupURL,
    purposeDone,
    purposeRunning,
    purposeTitle,
    whereSentences,
  } from "./processing-summary.js";
  import type {
    CoverageReport,
    Node,
    ProcessingJob,
    ProcessingPlan,
    ProcessingStatus,
  } from "./generated/docbank.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    onrendition: (attachmentID: string) => void;
  }

  let { session, node, path, onclose, onauthfailure, onrendition }: Props = $props();

  type Option = { name: string; plan: ProcessingPlan };
  // Refusals about the file's type rather than this particular file.
  const unsupportedReasons = new Set(["unsupported_media_type", "unbounded_media_family"]);

  let options = $state<Option[]>([]);
  let profileName = $state("");
  let coverage = $state<CoverageReport | null>(null);
  let job = $state<ProcessingJob | null>(null);
  let status = $state<ProcessingStatus | null>(null);
  let consented = $state(false);
  let loading = $state(true);
  let running = $state(false);
  let error = $state("");
  let processingController: AbortController | null = null;
  let generation = 0;

  const selected = $derived(options.find((option) => option.name === profileName) ?? null);
  const plan = $derived(selected?.plan ?? null);
  const purpose = $derived(plan ? processingPurpose(plan) : "extract");
  const title = $derived(purposeTitle(purpose));
  const done = $derived(status?.state === "completed");
  const failed = $derived(status?.state === "failed" || !!status?.failure_code);
  const canRun = $derived(
    !!plan &&
      !plan.rendition_ineligible_reason &&
      !running &&
      !loading &&
      (!plan.consent_required || consented),
  );

  onMount(() => {
    void load();
    return () => {
      generation += 1;
      cancelProcessing();
    };
  });

  function cancelProcessing(): void {
    processingController?.abort();
    processingController = null;
    running = false;
  }

  function close(): void {
    generation += 1;
    cancelProcessing();
    onclose();
  }

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function planProfile(name: string): Promise<ProcessingPlan> {
    return generated.planDocumentProcessing(
      {
        selector: {
          node_id: node.id,
          content_version_id: node.current_version_id!,
          profile: name,
        },
      },
      { session },
    );
  }

  // Plans every profile so only the ones that accept this file's type are offered.
  async function load(keep = ""): Promise<void> {
    const request = ++generation;
    cancelProcessing();
    loading = true;
    error = "";
    job = null;
    status = null;
    coverage = null;
    try {
      const profiles = await generated.listDocumentProcessingProfiles({ session });
      if (request !== generation || !node.current_version_id) return;
      const planned = await Promise.allSettled(profiles.map((item) => planProfile(item.name)));
      if (request !== generation) return;
      const next: Option[] = [];
      for (const [index, result] of planned.entries()) {
        if (result.status === "rejected") {
          if (result.reason instanceof APIError && result.reason.status === 401)
            throw result.reason;
          continue;
        }
        if (unsupportedReasons.has(result.value.rendition_ineligible_reason ?? "")) continue;
        next.push({ name: profiles[index]!.name, plan: result.value });
      }
      const failure = planned.find((result) => result.status === "rejected");
      if (next.length === 0 && failure?.status === "rejected") throw failure.reason;
      options = next;
      profileName =
        next.find((option) => option.name === keep)?.name ??
        next.find((option) => !option.plan.consent_required)?.name ??
        next.find((option) => externalDestinations(option.plan).length === 0)?.name ??
        next[0]?.name ??
        "";
      consented = false;
      await loadCoverage(request);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function loadCoverage(request: number): Promise<void> {
    if (!plan || !node.current_version_id) return;
    try {
      const next = await generated.getDocumentProcessingCoverage(
        {
          profile: profileName,
          vault_uid: plan.vault_uid,
          content_version_id: [node.current_version_id],
        },
        { session },
      );
      if (request === generation) coverage = next;
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    }
  }

  function chooseProfile(name: string): void {
    if (name === profileName) return;
    const request = ++generation;
    cancelProcessing();
    profileName = name;
    consented = false;
    error = "";
    job = null;
    status = null;
    coverage = null;
    void loadCoverage(request);
  }

  async function execute(): Promise<void> {
    if (!plan || !canRun || !node.current_version_id) return;
    const request = generation;
    cancelProcessing();
    const active = new AbortController();
    processingController = active;
    running = true;
    error = "";
    job = null;
    status = null;
    try {
      const result = await startProcessing(
        session,
        plan.selector,
        plan.fingerprint,
        plan.profile_fingerprint,
        plan.consent_required,
        (event) => {
          if (processingController === active && event.job) job = event.job;
        },
        active.signal,
      );
      if (processingController !== active) return;
      job = result.job;
      status = result.status;
      const fresh = await planProfile(profileName);
      if (processingController !== active) return;
      options = options.map((option) =>
        option.name === profileName ? { ...option, plan: fresh } : option,
      );
      await loadCoverage(request);
    } catch (cause) {
      if (
        processingController === active &&
        !(cause instanceof DOMException && cause.name === "AbortError")
      ) {
        handleFailure(cause);
      }
    } finally {
      if (processingController === active) {
        processingController = null;
        running = false;
      }
    }
  }

  async function revokeConsent(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      await generated.revokeDocumentProcessingConsent({ session });
      if (request !== generation) return;
      await load(profileName);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function coverageSentence(report: CoverageReport | null): string {
    if (!report) return "";
    if (report.state === "complete") return "Done for this version.";
    if (report.state === "rebuilding") return "Updating. The previous result stays available.";
    const started =
      report.renditions.complete > 0 || report.embeddings.some((item) => item.complete > 0);
    return report.state === "partial" && started ? "Partly done for this version." : "";
  }

  function permissionWord(state: ProcessingPlan["consent_state"]): string {
    switch (state) {
      case "active":
        return "Allowed";
      case "required":
        return "Not yet allowed";
      case "expired":
        return "Expired";
      case "revoked":
        return "Revoked";
    }
  }

  function model(hop: ProcessingPlan["flow"][number]): string | undefined {
    const { model, model_revision } = hop.runtime_disclosure;
    if (!model) return undefined;
    return model_revision ? `${model}@${model_revision}` : model;
  }

  const facts = $derived.by<InspectorFact[]>(() => {
    if (!plan) return [];
    const hops = plan.flow.filter(
      (hop, index, flow) =>
        flow.findIndex(
          (other) =>
            other.provider_id === hop.provider_id &&
            other.runtime_disclosure.endpoint === hop.runtime_disclosure.endpoint,
        ) === index,
    );
    const seen = [...new Set(plan.flow.flatMap((hop) => hop.runtime_disclosure.metadata_classes))];
    const result: InspectorFact[] = [{ label: "Profile", value: profileName, mono: false }];
    hops.forEach((hop, index) => {
      const suffix = hops.length > 1 ? ` ${index + 1}` : "";
      const local = hop.trust_boundary === "local_process";
      result.push(
        { label: `Service${suffix}`, value: hop.provider_id, copy: true },
        {
          label: `Address${suffix}`,
          value: local ? "This computer" : hop.runtime_disclosure.endpoint,
          copy: !local,
        },
        { label: `Model${suffix}`, value: model(hop), copy: true },
        { label: `Deployment${suffix}`, value: hop.runtime_disclosure.deployment, copy: true },
      );
    });
    result.push(
      { label: "Sends", value: describeClasses(plan.disclosed_classes) },
      { label: "Service sees", value: describeClasses(seen) },
      { label: "Keeps", value: describeClasses(plan.retained_classes) },
      { label: "Size", value: formatBytes(plan.estimate.source_bytes) },
      { label: "Service calls", value: plan.estimate.provider_calls },
      {
        label: "Search vectors",
        value: plan.estimate.vector_spaces > 0 ? plan.estimate.vector_spaces : undefined,
      },
      {
        label: "Backups",
        value: plan.backup_consequence.replace(/^./, (char) => char.toUpperCase()),
      },
      { label: "Permission", value: permissionWord(plan.consent_state) },
      { label: "Plan ID", value: plan.fingerprint, copy: true },
      { label: "Profile ID", value: plan.profile_fingerprint, copy: true },
      { label: "Version", value: node.current_version_id, copy: true },
      { label: "Job ID", value: job?.id, copy: true },
    );
    if (coverage) {
      result.push({
        label: "Text",
        value: `${coverage.renditions.complete} of ${coverage.renditions.total} done`,
      });
      for (const item of coverage.embeddings)
        result.push({
          label: `Vectors ${item.name}`,
          value: `${item.complete} of ${item.total} done`,
        });
    }
    return result;
  });
</script>

<Modal
  {title}
  ariaLabel={title}
  width="520px"
  onclose={close}
  closeOnOverlayClick={!running}
  footer={plan ? actions : undefined}
>
  <div class="processing">
    <p class="file" title={path}>{path}</p>
    {#if loading && options.length === 0}
      <p class="muted"><Spinner size={14} /> Checking profiles…</p>
    {:else if error && options.length === 0}
      <p class="error" role="alert">{error}</p>
      <div><Button size="sm" onclick={() => void load()}>Try again</Button></div>
    {:else if options.length === 0}
      <p>
        No processing profile handles this file type. <a
          href={processingSetupURL}
          target="_blank"
          rel="noreferrer">Set one up</a
        >
      </p>
    {:else if plan}
      {#if options.length > 1}
        <div class="profile">
          <span>Profile</span>
          <SelectDropdown
            title="Profile"
            value={profileName}
            disabled={running || loading}
            options={options.map((option) => ({ value: option.name, label: option.name }))}
            onchange={chooseProfile}
          />
        </div>
      {/if}
      <ul class="summary">
        {#each whereSentences(plan) as sentence (sentence)}
          <li class:external={sentence.startsWith("Sends")}>
            {#if sentence.startsWith("Sends")}<SendIcon
                size={15}
                aria-hidden="true"
              />{:else}<HardDriveIcon size={15} aria-hidden="true" />{/if}
            <span>{sentence}</span>
          </li>
        {/each}
        <li>
          <ArchiveIcon size={15} aria-hidden="true" /><span>{keptSentence(plan, purpose)}</span>
        </li>
      </ul>
      {#if plan.rendition_ineligible_reason}
        <p class="warning">{ineligibleSentence(plan.rendition_ineligible_reason)}</p>
      {:else if plan.consent_required}
        <Checkbox
          bind:checked={consented}
          disabled={running}
          label={consentLabel(profileName, plan)}
        />
        {#if plan.estimate.scope}<p class="muted">{plan.estimate.scope}</p>{/if}
      {/if}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div aria-live="polite" class="outcome">
        {#if running}
          <!-- The run button shows progress. -->
        {:else if failed}
          <p class="error">
            Failed{status?.failure_code ? `: ${status.failure_code.replaceAll("_", " ")}` : "."}
          </p>
        {:else if done}
          <p>{purposeDone(purpose)}</p>
        {:else if job}
          <p class="muted">Running in the background.</p>
        {:else if coverageSentence(coverage)}
          <p class="muted">{coverageSentence(coverage)}</p>
        {/if}
      </div>
      <details class="details">
        <summary>Details</summary>
        <div class="facts"><InspectorDetails {facts} label="Processing details" /></div>
        <Button
          size="sm"
          surface="soft"
          disabled={running || loading}
          onclick={() => void revokeConsent()}>Revoke all processing permissions</Button
        >
      </details>
    {/if}
  </div>
</Modal>

{#snippet actions()}
  {#if job?.attachment_id && done}
    <Button size="sm" surface="soft" onclick={() => onrendition(job!.attachment_id!)}
      >{purpose === "transcribe" ? "Read transcript" : "Read text"}</Button
    >
  {/if}
  {#if plan}
    <Button size="sm" tone="info" surface="solid" disabled={!canRun} onclick={() => void execute()}>
      {#if running}<Spinner size={14} /> {purposeRunning(purpose)}{:else}{title}{/if}
    </Button>
  {/if}
{/snippet}

<style>
  .processing {
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }
  p {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  .file {
    overflow: hidden;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .muted {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-muted);
  }
  a {
    color: var(--accent-blue);
  }
  .error {
    color: var(--accent-red);
  }
  .warning {
    color: var(--accent-amber);
  }
  .profile {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .profile > span {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .profile :global(.kit-select-dropdown) {
    min-width: 200px;
  }
  .summary {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .summary li {
    display: grid;
    grid-template-columns: 16px minmax(0, 1fr);
    gap: var(--space-2);
    align-items: start;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  .summary li :global(svg) {
    margin-top: 3px;
    color: var(--text-muted);
  }
  .summary li.external :global(svg) {
    color: var(--accent-amber);
  }
  .summary span {
    overflow-wrap: anywhere;
  }
  .outcome:empty {
    display: none;
  }
  .details {
    display: grid;
    gap: var(--space-3);
  }
  .details summary {
    width: fit-content;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  .details[open] {
    gap: var(--space-3);
  }
  .details[open] > :global(*:not(summary)) {
    margin-top: var(--space-3);
  }
  .facts :global(.inspector-details > div) {
    grid-template-columns: 112px minmax(0, 1fr);
  }
</style>
