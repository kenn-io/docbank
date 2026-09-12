<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { Button, SelectDropdown } from "@kenn-io/kit-ui";
  import { APIError, type Node, type ContentVersion } from "./api.js";
  import { retainedEmailPDFs, renderEmailPDF, type EmailPDFReceipt } from "./email-pdf.js";
  import { offerPreparedDownload, prepareEmailPDFDownload } from "./download.js";

  let { session, node, version, onauthfailure }: { session: string; node: Node; version: ContentVersion; onauthfailure: (cause: unknown) => void } = $props();
  let paper = $state("A4");
  let busy = $state(false);
  let message = $state("");
  let receipts = $state<EmailPDFReceipt[]>([]);
  const controller = new AbortController();
  const selected = $derived(receipts.filter((r) => r.binding.recipe.paper === paper));
  onDestroy(() => controller.abort());
  onMount(() => { void retainedEmailPDFs(session, version.id, controller.signal).then((found) => { receipts = found; }).catch(report); });
  function report(cause: unknown): void {
    if (controller.signal.aborted) return;
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    else message = cause instanceof Error ? cause.message : String(cause);
  }
  async function download(receipt: EmailPDFReceipt): Promise<void> {
    busy = true;
    message = "Verifying complete retained PDF…";
    try {
      const ready = await prepareEmailPDFDownload(session, node, version, receipt, controller.signal, () => undefined);
      offerPreparedDownload(ready);
      message = `Verified ${receipt.output.pages} pages; browser save started.`;
    } catch (cause) { report(cause); }
    finally { busy = false; }
  }
  async function render(): Promise<void> {
    busy = true;
    message = "Rendering this exact email version…";
    try {
      const receipt = await renderEmailPDF(session, version.id, paper, controller.signal);
      if (!receipts.some((r) => r.attachment_id === receipt.attachment_id)) receipts = [...receipts, receipt];
      await download(receipt);
    } catch (cause) { report(cause); }
    finally { busy = false; }
  }
</script>

<section aria-label="Exact email PDF" class="email-pdf">
  <strong>Email PDF</strong>
  <p>Complete body and quoted text, verified inline images, and attachment inventory. Original EML stays unchanged.</p>
  <SelectDropdown title="PDF paper" value={paper} options={[{ value: "A4", label: "A4 portrait" }, { value: "Letter", label: "US Letter portrait" }]} disabled={busy} onchange={(value) => { paper = value; }} />
  {#each selected as receipt (receipt.attachment_id)}
    <Button size="sm" disabled={busy} onclick={() => void download(receipt)}>Download retained PDF · {receipt.output.pages} pages · {receipt.profile_fingerprint.slice(0, 8)}</Button>
  {/each}
  <Button size="sm" disabled={busy} onclick={() => void render()}>Render PDF with configured recipe</Button>
  <p role="status">{message}</p>
</section>

<style>
  .email-pdf { display: grid; gap: 0.65rem; margin-top: 1rem; }
  p { margin: 0; font-size: 0.8rem; }
</style>
