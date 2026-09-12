<script lang="ts">
  import { onMount } from "svelte";
  import { Button, SelectDropdown } from "@kenn-io/kit-ui";
  import { APIError, requestJSON, type Node } from "./api.js";
  import type { SelectedSource } from "./selectedSource.js";
  import EmailPDFButton from "./EmailPDFButton.svelte";
  import { emailFrameDocument } from "./email-frame.js";
  import { readEmailMetadata, readEmailPart, prepareEmailHTML, validFrameEscape, type EmailMetadata } from "./email-viewer.js";

  let { session, source, authorizationRevision, onattachments, onauthfailure }: {
    session:string; source:SelectedSource; authorizationRevision:number;
    onattachments:() => void; onauthfailure:(cause:unknown) => void;
  } = $props();
  let metadata = $state<EmailMetadata>(); let node = $state<Node>();
  let selected = $state(""); let raw = $state(""); let showRaw = $state(false);
  let error = $state(""); let bodyError = $state(""); let loading = $state(true);
  let text = $state(""); let frame = $state<{ html:string; nonce:string }>();
  let warnings = $state<string[]>([]); let frameReady = $state(false);
  let iframe = $state<HTMLIFrameElement>(); let escapeButton = $state<HTMLButtonElement>();
  const inventory = $derived(metadata?.evidence.inventory);
  const message = $derived(inventory?.messages.find((m) => m.path === inventory.root_path));
  const alternatives = $derived(message?.alternatives ?? []);
  const selectedAlternative = $derived(alternatives.find((a) => a.part_path === selected));
  const inlineParts = $derived(inventory?.parts.filter((p) => p.content_id.value || p.disposition === "inline") ?? []);
  const diagnostics = $derived([...(inventory?.diagnostics ?? []),...(message?.diagnostics ?? []),...(inventory?.parts.flatMap((p) => p.diagnostics) ?? [])]);

  function report(cause:unknown): string {
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  $effect(() => {
    const identity = `${session}:${source.key}:${authorizationRevision}`; void identity;
    const controller = new AbortController(); let current = true;
    metadata = undefined; node = undefined; frame = undefined; text = ""; raw = ""; error = ""; loading = true;
    void (async () => {
      const result = await readEmailMetadata(session,source,controller.signal);
      const root = result.evidence.inventory!.parts.find((p) => p.path === result.evidence.inventory!.root_path)!;
      const headers = root.header_block ? await readEmailPart(session,result,root,root.header_block,controller.signal) : undefined;
      const live = await requestJSON<Node>(`/api/v1/nodes/${source.nodeID}`,session,{ signal:controller.signal });
      if (!current) return;
      if (live.id !== source.nodeID || live.kind !== "file") throw new Error("Email access identity changed.");
      node = { ...live, revision:authorizationRevision };
      if (headers) {
        try { raw = new TextDecoder("utf-8",{ fatal:true }).decode(headers); }
        catch { raw = "Invalid UTF-8 bytes shown as \\xNN; original bytes remain in the EML.\n" + Array.from(headers,(byte) => byte < 128 ? String.fromCharCode(byte) : `\\x${byte.toString(16).padStart(2,"0")}`).join(""); }
      } else raw = "Raw headers unavailable; download the original EML.";
      metadata = result;
      const message = result.evidence.inventory!.messages.find((m) => m.path === result.evidence.inventory!.root_path);
      selected = message?.alternatives.find((a) => a.kind === "html" && a.display_state === "available")?.part_path ?? message?.selected_body_path ?? message?.alternatives[0]?.part_path ?? "";
    })().catch((cause:unknown) => { if (current) error = report(cause); }).finally(() => { if (current) loading = false; });
    return () => { current = false; controller.abort(); frame = undefined; text = ""; };
  });
  $effect(() => {
    const alternative = selectedAlternative; const exact = metadata;
    const controller = new AbortController(); let current = true;
    frame = undefined; frameReady = false; text = ""; bodyError = ""; warnings = [];
    if (exact && alternative) {
      void (async () => {
        if (!alternative.display || alternative.display_state !== "available") throw new Error(`This body is ${alternative.display_state}. Download the original for complete content.`);
        if (alternative.kind === "html") {
          const result = await prepareEmailHTML(session,exact,alternative,controller.signal);
          if (!current) return;
          const nonce = crypto.randomUUID();
          frame = { html:emailFrameDocument(result.html,nonce,location.origin), nonce };
          warnings = result.warnings;
        } else {
          const part = exact.evidence.inventory!.parts.find((p) => p.path === alternative.part_path)!;
          const bytes = await readEmailPart(session,exact,part,alternative.display,controller.signal);
          if (current) text = new TextDecoder("utf-8",{ fatal:true }).decode(bytes);
        }
      })().catch((cause:unknown) => { if (current) bodyError = report(cause); });
    }
    return () => { current = false; controller.abort(); frame = undefined; text = ""; };
  });
  onMount(() => {
    const receive = (event:MessageEvent) => { if (frame && validFrameEscape(event,iframe?.contentWindow,frame.nonce)) escapeButton?.focus(); };
    window.addEventListener("message",receive); return () => window.removeEventListener("message",receive);
  });
</script>

<section aria-label="Email reader" class="email-reader">
  <div class="heading"><strong>Archived email</strong><span>Exact selected version</span></div>
  <p class="hint">Remote resources and sender scripts are blocked. HTML is a safe reading copy; the original EML remains available below.</p>
  {#if loading}<p role="status">Verifying email headers and MIME authority…</p>
  {:else if error}<p role="status">{error}</p>
  {:else if message && metadata}
    <dl aria-label="Decoded email headers">
      {#each Object.entries(message.fields) as [name, fields]}
        {#if fields.length}
          <dt>{name.replaceAll("_"," ")}</dt><dd>{#each fields as field}<div>{field.text ?? `(${field.state})`}{#if field.state !== "decoded"} · {field.state}{/if}</div>{/each}</dd>
        {:else if ["subject","from","to"].includes(name)}<dt>{name}</dt><dd>(missing)</dd>{/if}
      {/each}
      <dt>Date</dt><dd>{message.date.civil ?? message.date.state} {message.date.timezone_text ?? ""} · {message.date.state}{#if message.date.timezone_state === "unknown_named"} (unknown timezone){/if}</dd>
    </dl>
    <Button size="sm" surface="soft" onclick={() => { showRaw = !showRaw; }}>{showRaw ? "Hide raw headers" : "Show raw headers"}</Button>
    {#if showRaw}<pre aria-label="Raw email headers">{raw}</pre>{/if}
    <p role="status">MIME inventory: {inventory?.state}. Body and inline bytes are SHA-256 verified before display.</p>
    {#if diagnostics.length}
      <details><summary>MIME warnings ({diagnostics.length})</summary>
        <ul>{#each diagnostics as diagnostic}<li>{diagnostic.path ? `Part ${diagnostic.path} · ` : ""}{diagnostic.code}: {diagnostic.detail}</li>{/each}</ul>
      </details>
    {/if}
    {#if alternatives.length}
      <SelectDropdown title="Email body" value={selected} options={alternatives.map((a) => ({ value:a.part_path,label:`${a.kind === "html" ? "HTML" : "Plain text"} · part ${a.part_path} · ${a.display_state}` }))} onchange={(value) => { selected = value; }} />
    {:else}<p role="status">No displayable body. Original MIME remains available.</p>{/if}
    {#if bodyError}<p role="status">{bodyError}</p>{/if}
    {#if warnings.length}<details><summary>Reading-copy changes ({warnings.length})</summary><ul>{#each warnings as warning}<li>{warning}</li>{/each}</ul></details>{/if}
    {#if frame}
      <button class="escape" bind:this={escapeButton} type="button" onclick={() => iframe?.focus()}>Enter email body · Escape returns here</button>
      {#key frame.nonce}
        <iframe bind:this={iframe} title={`Email HTML body of ${source.name}`} sandbox="allow-scripts" referrerpolicy="no-referrer" srcdoc={frame.html} onload={() => { frameReady = true; }}></iframe>
      {/key}
      {#if !frameReady}<p role="status">Loading isolated email body…</p>{/if}
    {:else if selectedAlternative?.kind === "plain" && !bodyError}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex (keyboard access to the scrollable complete body) -->
      <pre class="plain-body" aria-label="Plain text email body" tabindex="0">{text}</pre>
    {/if}
    {#if inlineParts.length}<details><summary>Inline MIME resources ({inlineParts.length})</summary><ul>{#each inlineParts as part}<li>Part {part.path}: {part.filename.safe_name || part.content_id.value || "Unnamed inline part"} · {part.decode_state}</li>{/each}</ul><p>Inline display supports PNG and JPEG, up to 5 MiB and 8 million pixels each, 128 occurrences, 16 MiB and 32 million pixels total. Other resources retain placeholders and their original MIME bytes.</p></details>{/if}
    <Button size="sm" surface="soft" onclick={onattachments}>Inspect attachment documents</Button>
    {#if node}<p class="hint">New PDF rendering requires a configured, pinned Chromium bundle and fonts on a Linux daemon with systemd isolation. Retained PDFs remain downloadable without a renderer.</p><EmailPDFButton {session} {node} version={metadata.version} {onauthfailure} />{/if}
  {/if}
</section>

<style>
  .email-reader { display:grid; gap:var(--space-3); min-width:0; font-size:var(--font-size-sm); }
  .heading { display:flex; gap:var(--space-3); justify-content:space-between; }
  .heading span, .hint { color:var(--text-muted); font-size:var(--font-size-xs); }
  p { margin:0; }
  dl { display:grid; grid-template-columns:6rem minmax(0,1fr); gap:var(--space-2); margin:0; }
  dt { color:var(--text-muted); text-transform:capitalize; }
  dd { margin:0; overflow-wrap:anywhere; }
  pre { margin:0; padding:var(--space-3); white-space:pre-wrap; overflow-wrap:anywhere; max-height:360px; overflow:auto; background:var(--bg-inset); border:1px solid var(--border-default); border-radius:var(--radius-md); font-size:var(--font-size-xs); }
  .plain-body { max-height:560px; min-height:160px; }
  iframe { width:100%; height:560px; border:1px solid var(--border-default); border-radius:var(--radius-md); background:white; }
  summary { cursor:pointer; }
  ul { padding-left:1.2rem; overflow-wrap:anywhere; }
  .escape { background:var(--bg-inset); color:var(--text-primary); border:1px solid var(--border-default); border-radius:var(--radius-md); padding:var(--space-2); cursor:pointer; }
</style>
