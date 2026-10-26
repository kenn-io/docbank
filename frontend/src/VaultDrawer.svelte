<script lang="ts">
  import { setContext, untrack } from "svelte";
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
  .usage {
    display: grid;
    justify-items: start;
    gap: var(--space-5);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }
  p {
    margin: 0;
    max-width: 72ch;
    color: var(--text-primary);
  }
</style>
