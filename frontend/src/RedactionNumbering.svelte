<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { Button, Checkbox, Modal, SelectDropdown, Spinner, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { listBatesNamespaces, createBatesNamespace, downloadBatesExport } from "./bates.js";
  import {
    numberedLabels,
    readBatesNamespaceDetail,
    reserveNumberedRedaction,
    publishNumberedRedaction,
    type NumberingInputs,
    type FrozenNumbering,
  } from "./redactionNumbering.js";
  import type { BatesNamespace, BatesExport, NumberedReservation } from "./generated/docbank.js";
  let {
    session,
    inputs,
    title,
    locked = $bindable(false),
    onauthfailure,
    onpublished,
    onproductions,
  }: {
    session: string;
    inputs: NumberingInputs;
    title: string;
    locked?: boolean;
    onauthfailure: (cause: unknown) => void;
    onpublished: () => void;
    onproductions?: () => void;
  } = $props();
  let opened = $state(false),
    namespaces = $state<BatesNamespace[]>([]),
    selected = $state(""),
    start = $state(""),
    cursorReady = $state(false),
    loading = $state(false),
    consent = $state(false),
    confirm = $state(false);
  let prefix = $state(""),
    suffix = $state(""),
    padding = $state("6");
  let error = $state(""),
    notice = $state(""),
    phase = $state<
      "idle" | "reserving" | "reserve-failed" | "publishing" | "publish-failed" | "done"
    >("idle");
  let frozen = $state<FrozenNumbering>(),
    reservation = $state<NumberedReservation>(),
    artifact = $state<BatesExport>();
  let controller = new AbortController(),
    lookup = new AbortController(),
    disposed = false;
  let listing = $state(false);
  const namespace = $derived(namespaces.find((n) => n.namespace_id === selected));
  const labels = $derived(
    namespace ? numberedLabels(namespace, Number(start), inputs.review.resolved.page_count) : [],
  );
  const busy = $derived(phase === "reserving" || phase === "publishing");
  $effect(() => {
    locked = !!frozen && !artifact;
  });
  function fail(cause: unknown) {
    if (disposed) return;
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }
  async function choose(id: string) {
    lookup.abort();
    lookup = new AbortController();
    const signal = lookup.signal;
    selected = id;
    start = "";
    cursorReady = false;
    error = "";
    loading = true;
    try {
      const detail = await readBatesNamespaceDetail(session, id, signal);
      if (signal.aborted || disposed || selected !== id) return;
      const i = namespaces.findIndex((n) => n.namespace_id === id);
      if (i >= 0) namespaces = namespaces.map((n, j) => (j === i ? detail.namespace : n));
      start = String(detail.next_sequence);
      cursorReady = true;
    } catch (cause) {
      if (!signal.aborted) fail(cause);
    } finally {
      if (!signal.aborted && !disposed) loading = false;
    }
  }
  async function open() {
    opened = true;
    loading = true;
    error = "";
    listing = true;
    try {
      const result = await listBatesNamespaces(session, controller.signal);
      if (disposed) return;
      namespaces = result;
      listing = false;
      if (result[0]) await choose(result[0].namespace_id);
    } catch (cause) {
      if (!controller.signal.aborted) fail(cause);
    } finally {
      if (!disposed) loading = false;
    }
  }
  async function create() {
    error = "";
    loading = true;
    try {
      const ns = await createBatesNamespace(
        session,
        prefix,
        suffix,
        Number(padding),
        controller.signal,
      );
      if (disposed) return;
      if (!namespaces.some((n) => n.namespace_id === ns.namespace_id))
        namespaces = [...namespaces, ns];
      await choose(ns.namespace_id);
    } catch (cause) {
      if (!controller.signal.aborted) fail(cause);
    } finally {
      if (!disposed) loading = false;
    }
  }
  function begin() {
    if (!namespace || !cursorReady || !consent || !labels.length || frozen) return;
    frozen = structuredClone({
      ...$state.snapshot(inputs),
      namespace: $state.snapshot(namespace),
      start: Number(start),
    });
    confirm = false;
    void run();
  }
  async function run() {
    const request = frozen;
    if (!request || busy) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    error = "";
    notice = "";
    try {
      if (!reservation) {
        phase = "reserving";
        const result = await reserveNumberedRedaction(session, request, signal);
        if (disposed || signal.aborted) return;
        reservation = result;
      }
      phase = "publishing";
      const result = await publishNumberedRedaction(session, request, reservation, signal);
      if (disposed || signal.aborted) return;
      artifact = result;
      phase = "done";
      onpublished();
    } catch (cause) {
      if (disposed || controller.signal !== signal) return;
      if (
        !reservation &&
        cause instanceof APIError &&
        [400, 404, 409, 413, 422].includes(cause.status)
      ) {
        frozen = undefined;
        phase = "idle";
        consent = false;
        fail(cause);
      } else {
        phase = reservation ? "publish-failed" : "reserve-failed";
        if (signal.aborted)
          error = reservation
            ? "Publication interrupted. Retry this allocation; its numbers remain reserved."
            : "Reservation reply is uncertain. Retry the same range before changing it.";
        else fail(cause);
      }
    }
  }
  async function download() {
    if (!artifact) return;
    error = "";
    try {
      await downloadBatesExport(session, artifact, controller.signal);
      if (!disposed) notice = "Download started. Keep this tab open until it finishes.";
    } catch (cause) {
      if (!controller.signal.aborted) fail(cause);
    }
  }
  onMount(() => {
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape" && confirm) {
        event.preventDefault();
        event.stopImmediatePropagation();
        confirm = false;
      }
    };
    window.addEventListener("keydown", escape, true);
    return () => window.removeEventListener("keydown", escape, true);
  });
  function cancel() {
    controller.abort();
    phase = reservation ? "publish-failed" : "reserve-failed";
    error = reservation
      ? "Publication interrupted. Retry this allocation; its numbers remain reserved."
      : "Reservation reply is uncertain. Retry the same range before changing it.";
  }
  onDestroy(() => {
    disposed = true;
    controller.abort();
    lookup.abort();
  });
</script>

<section class="numbering" aria-label="Numbered PDF">
  {#if !opened}<Button onclick={() => void open()}>Number and keep PDF</Button>
  {:else}
    <h3>Keep a numbered PDF</h3>
    {#if artifact}
      <p><strong>{artifact.pages[0]?.label}–{artifact.pages.at(-1)?.label}</strong></p>
      <p>Kept in Productions. The unredacted original is retained indefinitely.</p>
      <Button tone="info" surface="solid" onclick={() => void download()}
        >Download numbered PDF</Button
      >
      {#if onproductions}<Button onclick={onproductions}>Open Productions</Button>{/if}
      <p class="muted">
        Download checks the retained file record. Masks and approved text are not rechecked.
      </p>
    {:else if frozen}
      <p>
        <strong
          >{numberedLabels(
            frozen.namespace,
            frozen.start,
            frozen.review.resolved.page_count,
          )[0]}–{numberedLabels(
            frozen.namespace,
            frozen.start,
            frozen.review.resolved.page_count,
          ).at(-1)}</strong
        >
      </p>
      {#if busy}<p role="status">
          <Spinner size={16} />{phase === "reserving"
            ? "Reserving numbers…"
            : "Rendering and keeping PDF…"}
        </p>
        <Button onclick={cancel}>Cancel request</Button>
      {:else}<p>
          {reservation
            ? "Numbers are reserved. Retry publication with this allocation."
            : "The reservation reply is uncertain. Retry the identical request."}
        </p>
        <Button onclick={() => void run()}
          >{reservation ? "Retry publication" : "Retry reservation"}</Button
        >{/if}
      {#if reservation}<p class="muted">Allocation: {reservation.allocation_id}</p>{/if}
    {:else}
      {#if loading}<p role="status"><Spinner size={16} />Loading namespace…</p>{/if}
      {#if namespaces.length}
        <SelectDropdown
          title="Numbering namespace"
          value={selected}
          options={namespaces.map((n) => ({
            value: n.namespace_id,
            label: n.prefix + "0".repeat(n.padding) + n.suffix,
          }))}
          disabled={loading}
          onchange={(id) => void choose(id)}
        />
        <label for="redaction-first-number">First number</label>
        <TextInput
          id="redaction-first-number"
          ariaLabel="First number"
          bind:value={start}
          disabled={!cursorReady || loading}
          block
        />
        <Button size="sm" disabled={loading} onclick={() => void choose(selected)}
          >Refresh next number</Button
        >
        {#if cursorReady}
          {#if labels.length}<p><strong>{labels[0]}–{labels.at(-1)}</strong></p>
          {:else}<p class="error">
              This range does not fit. Choose another first number or namespace.
            </p>{/if}
        {/if}
      {/if}
      {#if listing && !loading}<Button onclick={() => void open()}>Retry namespaces</Button>{/if}
      <details>
        <summary>Create or find a namespace</summary>
        <label for="numbering-prefix">Prefix</label><TextInput
          id="numbering-prefix"
          ariaLabel="Numbering prefix"
          bind:value={prefix}
          disabled={loading}
          block
        />
        <label for="numbering-suffix">Suffix</label><TextInput
          id="numbering-suffix"
          ariaLabel="Numbering suffix"
          bind:value={suffix}
          disabled={loading}
          block
        />
        <SelectDropdown
          title="Numbering digits"
          value={padding}
          options={Array.from({ length: 10 }, (_, i) => ({
            value: String(i + 1),
            label: `${i + 1} digits`,
          }))}
          disabled={loading}
          onchange={(v) => (padding = v)}
        />
        <Button disabled={loading} onclick={() => void create()}>Create namespace</Button>
      </details>
      <p>
        The unredacted original is kept indefinitely, with no release path. Failed or abandoned
        reservations keep their numbers.
      </p>
      <p>
        Each page gains a half-inch footer below its reviewed content. The numbered download is PDF
        only; download the plain ZIP before preview expiry if you need its text sidecar.
      </p>
      <p>
        Save the private review files before continuing. If this tab closes before publication,
        finishing a reserved range requires those files through the CLI.
      </p>
      <Checkbox bind:checked={consent} label="Keep the unredacted original indefinitely" />
      <Button
        tone="info"
        surface="solid"
        disabled={loading || !cursorReady || !consent || !labels.length}
        onclick={() => (confirm = true)}>Review numbering</Button
      >
    {/if}
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    {#if notice}<p role="status">{notice}</p>{/if}
  {/if}
</section>
{#if confirm}<Modal title="Keep this numbered PDF?" onclose={() => (confirm = false)}>
    <p>{title} · {inputs.review.resolved.page_count} pages</p>
    <p><strong>{labels[0]}–{labels.at(-1)}</strong></p>
    <p>
      The unredacted original will be kept indefinitely. Numbers cannot be released. Pages gain a
      half-inch footer below the reviewed content.
    </p>
    {#snippet footer()}<Button onclick={() => (confirm = false)}>Keep reviewing</Button><Button
        tone="info"
        surface="solid"
        onclick={begin}>Reserve and keep PDF</Button
      >{/snippet}
  </Modal>{/if}

<style>
  .numbering {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
    border-top: 1px solid var(--border-default);
    padding-top: var(--space-3);
  }
  h3,
  p {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  p {
    overflow-wrap: anywhere;
  }
  .muted {
    color: var(--text-muted);
  }
  .error {
    color: var(--status-error-text);
  }
  details {
    display: grid;
    gap: var(--space-2);
  }
</style>
