<script module lang="ts">
  // Setup outlives the tab, so leaving the drawer never stops a download or the save.
  let setup: { session: string; done: Promise<void> } | undefined;
</script>

<script lang="ts">
  import { setContext, untrack } from "svelte";
  import { Button, SearchByMeaningRow, Spinner, type SearchByMeaningState } from "@kenn-io/kit-ui";
  import * as generated from "./generated/docbank.js";
  import { APIError } from "./api-transport.js";
  import DrawerShell from "./DrawerShell.svelte";
  import BackupDrawer from "./BackupDrawer.svelte";
  import StorageDrawer from "./StorageDrawer.svelte";
  import JobsDrawer from "./JobsDrawer.svelte";
  import AuditEvidenceDrawer from "./AuditEvidenceDrawer.svelte";
  import TabStrip from "./TabStrip.svelte";
  import { drawerNavigation } from "./drawerNavigation.js";

  let {
    session,
    initialTab = "storage",
    onclose,
    onauthfailure,
  }: {
    session: string;
    initialTab?: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let tab = $state(untrack(() => initialTab));
  let switched = false;
  const options = [
    { value: "storage", label: "Disk use" },
    { value: "jobs", label: "Running now" },
    { value: "backups", label: "Backups" },
    { value: "auditEvidence", label: "Protected folders" },
    { value: "telemetry", label: "Usage reporting" },
    { value: "searchByMeaning", label: "Search by meaning" },
  ];
  setContext(drawerNavigation, {
    tabs,
    get switched() {
      return switched;
    },
  });

  function select(value: string): void {
    switched = true;
    tab = value;
  }

  let meaning = $state<generated.LocalTextStatus>();
  let meaningPending = $state(false);
  let meaningError = $state<{ message: string; retry: () => void }>();
  let meaningRevision = $state(0);
  const meaningRow = $derived(rowState(meaning, meaningPending));

  // Only status reads stop when the tab closes; a running setup is followed again on return.
  $effect(() => {
    if (tab !== "searchByMeaning") return;
    void meaningRevision;
    const controller = new AbortController();
    void followSetup(session, controller.signal);
    return () => controller.abort();
  });

  async function followSetup(current: string, signal: AbortSignal): Promise<void> {
    const pending = setup?.session === current ? setup.done : undefined;
    meaningPending = !!pending;
    meaningError = undefined;
    let retry: () => void = () => void meaningRevision++;
    try {
      if (pending) {
        retry = turnOn;
        await pending;
        retry = () => void meaningRevision++;
      }
      const next = await generated.getLocalTextStatus({ session: current, signal });
      if (!signal.aborted) meaning = next;
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      meaningError = { message: cause instanceof Error ? cause.message : String(cause), retry };
    } finally {
      if (!signal.aborted) meaningPending = false;
    }
  }

  // Clicking Turn on is the consent, so the reviewed plan is applied without another prompt.
  function turnOn(): void {
    const current = session;
    const done = (async () => {
      const plan = await generated.planLocalText({ server: "auto" }, { session: current });
      await generated.applyLocalText(
        { server: "auto", plan_fingerprint: plan.fingerprint, consent: true },
        { session: current },
      );
    })();
    const release = () => {
      if (setup?.done === done) setup = undefined;
    };
    done.then(release, release);
    setup = { session: current, done };
    meaningRevision++;
  }

  function rowState(
    status: generated.LocalTextStatus | undefined,
    pending: boolean,
  ): SearchByMeaningState | undefined {
    if (pending || status?.installing) return { kind: "downloading", doneBytes: 0, totalBytes: 0 };
    if (!status) return undefined;
    if (status.restart_required) return { kind: "on" };
    if (status.active) return status.provider === "ollama" ? { kind: "ollama" } : { kind: "on" };
    return status.download_bytes > 0
      ? { kind: "off", downloadBytes: status.download_bytes }
      : { kind: "downloaded" };
  }
</script>

{#snippet tabs()}
  <TabStrip tabs={options} value={tab} ariaLabel="System sections" onchange={select} />
{/snippet}

{#if tab === "backups"}
  <BackupDrawer {session} {onclose} {onauthfailure} />
{:else if tab === "storage"}
  <StorageDrawer {session} {onclose} {onauthfailure} />
{:else if tab === "auditEvidence"}
  <AuditEvidenceDrawer {session} {onclose} {onauthfailure} />
{:else if tab === "jobs"}
  <JobsDrawer {session} {onclose} {onauthfailure} />
{:else if tab === "searchByMeaning"}
  <DrawerShell title="Search by meaning" {onclose}>
    <section class="meaning" aria-label="Search by meaning">
      {#if meaningRow}
        <div class="row">
          <SearchByMeaningRow
            state={meaningRow}
            itemNoun="documents"
            labels={meaning?.restart_required ? { on: "Restart required" } : { keepsIndex: "" }}
            readonly={meaningRow.kind === "ollama"}
            onturnon={turnOn}
            onusebuiltin={() => {}}
          />
        </div>
      {:else if !meaningError}
        <p class="loading"><Spinner size={16} /> Reading search status…</p>
      {/if}
      {#if meaningError}
        <p class="error" role="alert">{meaningError.message}</p>
        <Button size="sm" onclick={meaningError.retry}>Try again</Button>
      {:else if meaning?.restart_required && !meaningPending}
        <p>Run <code>docbank daemon restart</code> to finish.</p>
      {/if}
    </section>
  </DrawerShell>
{:else}
  <DrawerShell title="Usage reporting" {onclose}>
    <section class="usage" aria-label="Usage reporting">
      <p>
        Docbank sends a random vault ID, app version, operating system, install age, feature use,
        searches that find nothing, failed actions, and session length. Document content, filenames,
        paths, tags, and search queries stay private.
      </p>
      <p>
        To turn reporting off, set <code>DOCBANK_TELEMETRY_ENABLED=0</code> in the environment that
        starts the daemon, then run <code>docbank daemon restart</code>.
      </p>
    </section>
  </DrawerShell>
{/if}

<style>
  .usage,
  .meaning {
    display: grid;
    justify-items: start;
    gap: var(--space-5);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }
  .row {
    justify-self: stretch;
    max-width: 72ch;
  }
  p {
    margin: 0;
    max-width: 72ch;
    color: var(--text-primary);
  }
  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
  }
  .error {
    color: var(--accent-red);
  }
</style>
