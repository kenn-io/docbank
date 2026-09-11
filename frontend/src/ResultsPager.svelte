<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import type { SnapshotState } from "./snapshotState.js";
  import type { SnapshotPage } from "./snapshots.js";

  interface Props {
    page: SnapshotPage;
    offset: number;
    status: SnapshotState["status"];
    onpage: (direction: "previous" | "next") => void;
    onrunagain: () => void;
  }

  let { page, offset, status, onpage, onrunagain }: Props = $props();
  const start = $derived(page.total === 0 ? 0 : offset + 1);
  const end = $derived(offset + page.rows.length);
</script>

<nav class="pager" aria-label="Snapshot pages">
  <p role="status">
    {start}–{end} of {page.total.toLocaleString("en-US")} ·
    {page.total_bytes.toLocaleString("en-US")} bytes · observed <time datetime={page.observed_at}>{page.observed_at}</time>
  </p>
  <div class="actions">
    {#if status === "loading"}<Spinner size={14} />{/if}
    <Button size="sm" ariaLabel="Previous page" disabled={status !== "ready" || !page.previous_cursor}
      onclick={() => onpage("previous")}>Previous</Button>
    <Button size="sm" ariaLabel="Next page" disabled={status !== "ready" || !page.next_cursor}
      onclick={() => onpage("next")}>Next</Button>
    {#if status === "expired"}<Button size="sm" tone="info" onclick={onrunagain}>Run again</Button>{/if}
  </div>
</nav>

<style>
  .pager { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-3) var(--space-4); border-top: 1px solid var(--border-default); }
  p { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); font-variant-numeric: tabular-nums; }
  .actions { display: flex; align-items: center; gap: var(--space-2); }
  @media (max-width: 640px) { .pager { align-items: stretch; flex-direction: column; } .actions { justify-content: flex-end; } }
</style>
