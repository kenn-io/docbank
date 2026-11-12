<script module lang="ts">
  import type { LocalTextStatus } from "./generated/docbank.js";

  export type SearchByMeaningSetup = {
    status?: LocalTextStatus;
    readError: string;
    pending: boolean;
    error: string;
    notice?: string;
  };
</script>

<script lang="ts">
  import { setContext, untrack } from "svelte";
  import {
    Button,
    Modal,
    SearchByMeaningRow,
    Spinner,
    type SearchByMeaningState,
  } from "@kenn-io/kit-ui";
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
    searchByMeaning,
    onclose,
    onauthfailure,
    onreadsearchbymeaning,
    onturnonsearchbymeaning,
    onrestart,
  }: {
    session: string;
    initialTab?: string;
    searchByMeaning: SearchByMeaningSetup;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    onreadsearchbymeaning: () => () => void;
    onturnonsearchbymeaning: () => void;
    onrestart: () => Promise<void>;
  } = $props();
  let confirmRestart = $state(false);
  let restarting = $state(false);
  async function restart(): Promise<void> {
    restarting = true;
    await onrestart();
    restarting = false;
    confirmRestart = false;
  }
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

  const meaning = $derived(searchByMeaning.status);
  const meaningRow = $derived(rowState(meaning, searchByMeaning.pending));

  $effect(() => {
    if (tab === "searchByMeaning") return untrack(onreadsearchbymeaning);
  });

  function rowState(
    status: LocalTextStatus | undefined,
    pending: boolean,
  ): SearchByMeaningState | undefined {
    if (pending) return { kind: "downloading", doneBytes: 0, totalBytes: 0 };
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
            onturnon={onturnonsearchbymeaning}
            onusebuiltin={() => {}}
          />
        </div>
      {:else if !searchByMeaning.readError}
        <p class="loading"><Spinner size={16} /> Reading search status…</p>
      {/if}
      {#if searchByMeaning.error}
        <p class="error" role="alert">{searchByMeaning.error}</p>
      {/if}
      {#if searchByMeaning.notice}
        <div class="restart-notice">
          <p role="status">{searchByMeaning.notice}</p>
          {@render restartRecovery()}
        </div>
      {/if}
      {#if searchByMeaning.readError}
        <p class="error" role="alert">{searchByMeaning.readError}</p>
        <Button size="sm" onclick={() => onreadsearchbymeaning()}>Try again</Button>
      {/if}
      {#if meaning?.restart_required}
        {#if meaning.restart_available}
          <Button size="sm" onclick={() => (confirmRestart = true)}>Restart</Button>
        {:else}
          <p>
            If you started Docbank in a terminal, press Ctrl+C and repeat the original command. If
            an operator runs its service or container, ask them to restart it. Then reopen Docbank
            and sign in.
          </p>
        {/if}
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

{#if confirmRestart}
  <Modal
    title="Restart Docbank?"
    onclose={() => {
      if (!restarting) confirmRestart = false;
    }}
  >
    <div class="restart-confirmation">
      <p>Open tabs sign out. Uploads, exports, and command-line requests may be interrupted.</p>
      {@render restartRecovery()}
    </div>
    {#snippet footer()}
      <Button disabled={restarting} onclick={() => (confirmRestart = false)}>Cancel</Button>
      <Button tone="danger" surface="solid" disabled={restarting} onclick={restart}
        >{restarting ? "Restarting…" : "Restart"}</Button
      >
    {/snippet}
  </Modal>
{/if}

{#snippet restartRecovery()}
  <p>
    If restart stalls, run <code>docbank daemon restart</code> in the vault's terminal. To reopen,
    run <code>docbank web --no-browser</code> there and open the printed URL, or reload your public URL
    and sign in. Fix any command error and retry.
  </p>
{/snippet}

<style>
  .usage,
  .meaning {
    display: grid;
    justify-items: start;
    gap: var(--space-5);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }
  .restart-confirmation,
  .restart-notice {
    display: grid;
    gap: var(--space-4);
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
  .row + .error {
    margin-top: calc(-1 * var(--space-2));
  }
</style>
