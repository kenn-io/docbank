<script lang="ts">
  import { Modal } from "@kenn-io/kit-ui";
  import OriginalPreview from "./OriginalPreview.svelte";
  import type { SelectedSource } from "./selectedSource.js";
  let {
    session,
    source,
    authorizationRevision,
    onauthfailure,
    onclose,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    onauthfailure: (cause: unknown) => void;
    onclose: () => void;
  } = $props();
</script>

<Modal
  title={source.name}
  ariaLabel={`Read ${source.name}`}
  width="calc(100vw - 32px)"
  maxWidth="none"
  maxHeight="calc(100svh - 32px)"
  {onclose}
>
  <div class="reader-content">
    <OriginalPreview {session} {source} {authorizationRevision} {onauthfailure} expanded />
  </div>
</Modal>

<style>
  .reader-content {
    height: calc(100svh - 140px);
    overflow: auto;
    min-height: 200px;
  }
</style>
