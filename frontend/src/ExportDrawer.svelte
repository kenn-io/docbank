<script lang="ts">
  import { onDestroy, untrack, tick } from "svelte";
  import {
    Button,
    Checkbox,
    Chip,
    CopyButton,
    RadioGroup,
    SelectDropdown,
    Spinner,
    TextInput,
  } from "@kenn-io/kit-ui";
  import type { RadioGroupOption } from "@kenn-io/kit-ui";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import DrawerShell from "./DrawerShell.svelte";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { ExportSession, type ExportInput, type ExportState } from "./exportState.js";
  import {
    maxExportMembers,
    maxPhotoExportMembers,
    type RolePolicy,
    type ExportOptions,
  } from "./exports.js";
  import { formatBytes, formatDate } from "./format.js";
  import type { AttachmentPublicationChoice } from "./generated/docbank.js";

  interface Props {
    session: string;
    open: boolean;
    input: ExportInput | null;
    notice?: string;
    onclose: () => void;
    onauthfailure: (error: unknown) => void;
    onactivechange?: (active: boolean) => void;
    onprepareanother?: () => void;
  }
  let {
    session,
    open,
    input,
    notice = "",
    onclose,
    onauthfailure,
    onactivechange,
    onprepareanother,
  }: Props = $props();
  let view = $state<Readonly<ExportState>>({ status: "idle" });
  let photoFormat = $state("jpeg"),
    quality = $state("90"),
    longEdge = $state("");
  let includeMetadata = $state(true),
    removeGPS = $state(true);
  const photo = $derived(input && "photos" in input);
  const longEdgeValidationMessage =
    "Enter a whole number from 1 to 100,000 pixels, or leave Long edge blank.";
  const validLongEdge = $derived(
    longEdge === "" || (/^\d+$/.test(longEdge) && +longEdge >= 1 && +longEdge <= 100000),
  );
  const validQuality = $derived(
    photoFormat === "png" || (/^\d+$/.test(quality) && +quality >= 1 && +quality <= 100),
  );
  const validPhotoSettings = $derived(validQuality && validLongEdge);
  let basename = $state("docbank-bundle.zip");
  let original = $state("required"),
    text = $state("omit"),
    pages = $state("omit");
  let emailPDF = $state("omit"),
    attachments = $state("omit"),
    partial = $state("strict"),
    duplicates = $state("preserve"),
    packaging = $state("flat"),
    recipe = $state("");
  const controller = new ExportSession(
    untrack(() => session),
    (next) => (view = next),
  );
  let authFailureHandled = false;
  const options = [
    { value: "omit", label: "Skip" },
    { value: "required", label: "Required" },
    { value: "optional", label: "If available" },
  ];
  const memberLimit = $derived(photo ? maxPhotoExportMembers : maxExportMembers);
  const count = $derived(
    input
      ? "members" in input
        ? input.members.length
        : "snapshot" in input
          ? input.snapshot.total
          : input.total
      : 0,
  );
  const recipeOptions = $derived([
    { value: "", label: "Choose a retained recipe" },
    ...(view.recipes ?? []).map((choice) => ({
      value: choice.recipe_sha256,
      label: `${choice.paper} · Chromium ${choice.renderer_version} · ${choice.messages}/${count} messages${choice.ambiguous ? ` · ${choice.ambiguous} ambiguous` : ""}`,
    })),
  ]);
  // One group per message version. Radios that share a version stay exclusive.
  function publicationGroups(items: readonly AttachmentPublicationChoice[]) {
    const groups: { versionID: string; legend: string; options: RadioGroupOption[] }[] = [];
    for (const choice of items) {
      const option: RadioGroupOption = {
        value: choice.operation_id,
        label: `${choice.name} · ${choice.attachments} attachments · ${choice.state}. ${formatDate(choice.created_at)} · message ${choice.node_id} · version ${choice.version_id.slice(0, 8)} · ${choice.operation_id}`,
      };
      const existing = groups.find((group) => group.versionID === choice.version_id);
      if (existing) existing.options.push(option);
      else
        groups.push({
          versionID: choice.version_id,
          legend: `Attachment set for message ${choice.node_id}`,
          options: [option],
        });
    }
    return groups;
  }
  const busy = $derived(["preparing", "starting", "running"].includes(view.status));
  const admitted = $derived(view.active);
  const plan = $derived(admitted?.plan ?? view.reviewed?.plan);
  const receipt = $derived(admitted?.job?.receipt);
  const terminal = $derived(
    admitted?.job && ["completed", "failed", "canceled"].includes(admitted.job.state),
  );
  $effect(() => onactivechange?.(!!view.active));

  $effect(() => {
    const current = input;
    const policies: RolePolicy[] = [
      ["original", original],
      ["text", text],
      ["pages", pages],
    ]
      .filter(([, choice]) => choice !== "omit")
      .map(([role, choice]) => ({
        role: role!,
        ...(choice === "optional" ? { allow_unavailable: true } : {}),
      }));
    if (emailPDF !== "omit") {
      const optional = partial === "partial" ? { allow_unavailable: true } : {};
      policies.push({ role: "email_pdf", recipe_sha256: recipe, ...optional });
      if (attachments !== "omit") policies.push({ role: "attachment_original", ...optional });
      if (attachments === "pdf")
        policies.push({ role: "attachment_pdf", recipe_sha256: recipe, ...optional });
    }
    const planOptions: ExportOptions = {
      ...(!photo && duplicates !== "preserve"
        ? { duplicate_policy: "collapse_exact_content" }
        : {}),
      ...(!photo && packaging !== "flat"
        ? { volume_limits: { roles: packaging === "one" ? 1 : 1000, role_bytes: 512 * 2 ** 20 } }
        : {}),
    };
    if (current && "photos" in current) {
      policies.splice(0, policies.length, { role: "photo_rendered" });
      planOptions.photo_render = {
        format: photoFormat,
        quality: Number(quality),
        long_edge: longEdge === "" ? 0 : Number(longEdge),
        include_metadata: includeMetadata,
        remove_gps: includeMetadata && removeGPS,
      };
    }
    if (current) untrack(() => controller.choose(current, policies, planOptions));
  });
  $effect(() => {
    if (!open)
      untrack(() => {
        controller.close();
      });
    else
      untrack(() => {
        // A new photo export captures current membership, even when its query and count match.
        if (input && "photos" in input) controller.resetPreparation();
        if (view.active && view.status === "disconnected") void controller.reconnect();
      });
  });
  $effect(() => {
    const error = [view.error, view.problemsError].find(
      (error) => error instanceof APIError && error.status === 401,
    );
    if (error instanceof APIError && error.status === 401)
      untrack(() => {
        if (authFailureHandled) return;
        authFailureHandled = true;
        onauthfailure(error);
        close();
      });
  });
  onDestroy(() => controller.dispose());
  function close(): void {
    controller.close();
    onclose();
  }
  async function prepareAnother(): Promise<void> {
    const resetInput = photo || !!notice;
    await controller.clearFinished();
    if (!view.active && resetInput) onprepareanother?.();
  }
  let optionsOpen = $state(false);
  const optionSummary = $derived(
    [
      [
        ["Originals", original],
        ["text", text],
        ["page images", pages],
        ["email PDFs", emailPDF],
      ]
        .filter(([, choice]) => choice !== "omit")
        .map(([label]) => label)
        .join(", ") || "Nothing selected",
      duplicates === "collapse" ? "shared duplicates" : "",
      packaging === "flat" ? "one archive" : "ZIP volumes",
    ]
      .filter(Boolean)
      .join(" · "),
  );
  // "Download ZIP" freezes the plan, builds the archive, and offers it in one
  // step. It pauses on the frozen plan when outputs are unavailable or an
  // attachment set needs choosing, so those are seen before anything is built.
  let autoDownload = $state(false);
  const unavailable = $derived(
    !!(
      view.problems?.total ||
      view.problemsError ||
      view.reviewed?.plan.counts?.unavailable ||
      view.reviewed?.plan.counts?.unavailable_inventories
    ),
  );
  const paused = $derived(!admitted && !!view.reviewed && view.status === "ready" && unavailable);
  async function downloadZIP(): Promise<void> {
    autoDownload = true;
    await controller.preview();
    if (view.status !== "ready" || !view.reviewed || unavailable) {
      autoDownload = false;
      return;
    }
    await controller.start();
  }
  async function continueDownload(): Promise<void> {
    autoDownload = true;
    await controller.start();
  }
  $effect(() => {
    if (!autoDownload || !receipt || admitted?.job?.state !== "completed") return;
    autoDownload = false;
    untrack(() => void controller.download(basename));
  });
  async function findRecipes(): Promise<void> {
    recipe = "";
    await tick();
    await controller.discoverRecipes();
  }
</script>

{#snippet hash(label: string, value: string, testid: string, copyLabel: string)}
  <div class="hash">
    <dt>{label}</dt>
    <dd>
      <code data-testid={testid}>{value}</code><CopyButton text={value} ariaLabel={copyLabel} />
    </dd>
  </div>
{/snippet}

{#if open}
  <DrawerShell
    title={photo ? "Download photos" : "Download files"}
    ariaLabel="Verified export"
    closeLabel="Close export"
    onclose={close}
  >
    <div class="tool-body">
      {#if notice}<p role="alert" class="tool-error">{notice}</p>{/if}
      <section class="source" aria-label="Export source">
        <strong>{admitted?.label ?? input?.label ?? "No source selected"}</strong>
        <span class="tool-muted"
          >{(plan?.total ?? count).toLocaleString()}
          {photo ? "photo" : "document"}{(plan?.total ?? count) === 1 ? "" : "s"}</span
        >
      </section>
      {#if count > memberLimit}<p class="tool-error">
          Choose at most {memberLimit.toLocaleString()}
          {photo ? "photos" : "documents"}.
        </p>{/if}

      <div class="tool-grid">
        <div
          class="tool-field"
          class:tool-span-3={!photo || admitted}
          class:tool-span-all={photo && !admitted}
        >
          <label for="export-basename">ZIP filename</label>
          <TextInput id="export-basename" ariaLabel="ZIP filename" bind:value={basename} block />
        </div>
        {#if !admitted && photo}
          <div class="tool-field tool-span-2">
            <span class="tool-label">Format</span>
            <SelectDropdown
              title="Photo format"
              value={photoFormat}
              options={[
                { value: "jpeg", label: "JPEG" },
                { value: "png", label: "PNG" },
              ]}
              onchange={(value) => {
                photoFormat = value;
                if (value === "png") quality = "90";
              }}
            />
          </div>
          <div class="tool-field tool-span-2">
            <label for="photo-export-quality">JPEG quality</label>
            <TextInput
              id="photo-export-quality"
              ariaLabel="JPEG quality"
              bind:value={quality}
              placeholder="1–100"
              disabled={photoFormat !== "jpeg"}
              invalid={!validQuality}
              block
            />
          </div>
          <div class="tool-field tool-span-2">
            <label for="photo-export-edge">Long edge, pixels</label>
            <TextInput
              id="photo-export-edge"
              ariaLabel="Long edge, pixels"
              bind:value={longEdge}
              placeholder="Original size"
              invalid={!validLongEdge}
              block
            />
          </div>
          <div class="checks tool-span-all">
            <Checkbox
              label="Include image metadata"
              checked={includeMetadata}
              onchange={(checked) => (includeMetadata = checked)}
            />
            <Checkbox
              label="Remove GPS"
              checked={removeGPS}
              disabled={!includeMetadata}
              onchange={(checked) => (removeGPS = checked)}
            />
          </div>
        {/if}
      </div>

      {#if !admitted && !photo}
        <details class="tool-disclosure" bind:open={optionsOpen}>
          <summary
            ><ChevronRightIcon size="14" aria-hidden="true" /><span class="tool-label">Options</span
            ><span class="tool-disclosure-value">{optionSummary}</span></summary
          >
          <div class="tool-grid">
            <div class="tool-field tool-span-2">
              <span class="tool-label">Original files</span>
              <SelectDropdown
                title="Original export policy"
                value={original}
                {options}
                onchange={(value) => (original = value)}
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Retained text</span>
              <SelectDropdown
                title="Text export policy"
                value={text}
                {options}
                onchange={(value) => (text = value)}
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Page images</span>
              <SelectDropdown
                title="Pages export policy"
                value={pages}
                {options}
                onchange={(value) => (pages = value)}
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Email body PDFs</span>
              <SelectDropdown
                title="Email body PDF"
                value={emailPDF}
                options={[
                  { value: "omit", label: "Skip" },
                  { value: "include", label: "Include retained body PDFs" },
                ]}
                onchange={(value) => {
                  emailPDF = value;
                  if (value === "include") {
                    original = "omit";
                    packaging = "bounded";
                  }
                }}
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Duplicates</span>
              <SelectDropdown
                title="Duplicate outputs"
                value={duplicates}
                options={[
                  { value: "preserve", label: "Preserve every occurrence" },
                  { value: "collapse", label: "Share exact duplicate outputs" },
                ]}
                onchange={(value) => (duplicates = value)}
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Packaging</span>
              <SelectDropdown
                title="ZIP packaging"
                value={packaging}
                options={[
                  { value: "flat", label: "Single flat archive" },
                  { value: "bounded", label: "Up to 1,000 outputs / 512 MiB per volume" },
                  { value: "one", label: "One output / 512 MiB per volume" },
                ]}
                onchange={(value) => (packaging = value)}
              />
            </div>
            {#if emailPDF !== "omit"}
              <div class="tool-field tool-span-3">
                <span class="tool-label">Attachments</span>
                <SelectDropdown
                  title="Email attachment outputs"
                  value={attachments}
                  options={[
                    { value: "omit", label: "Body PDF only" },
                    { value: "original", label: "Include original attachments" },
                    { value: "pdf", label: "Originals + separate qualified PDFs" },
                  ]}
                  onchange={(value) => (attachments = value)}
                />
              </div>
              <div class="tool-field tool-span-3">
                <span class="tool-label">Unavailable outputs</span>
                <SelectDropdown
                  title="Partial email export"
                  value={partial}
                  options={[
                    { value: "strict", label: "Fail if any output is unavailable" },
                    { value: "partial", label: "Allow declared unavailable outputs" },
                  ]}
                  onchange={(value) => (partial = value)}
                />
              </div>
              <div class="tool-field tool-span-all">
                <span class="tool-label">Body PDF recipe</span>
                <div class="inline-control">
                  {#if view.recipes?.length}<SelectDropdown
                      title="Retained PDF recipe"
                      value={recipe}
                      options={recipeOptions}
                      onchange={(value) => (recipe = value)}
                    />{/if}
                  <Button
                    disabled={busy || count === 0 || count > memberLimit}
                    onclick={() => void findRecipes()}
                    >{view.recipes ? "Find again" : "Find retained PDF recipes"}</Button
                  >
                </div>
                {#if view.recipes && !view.recipes.length}<p class="tool-muted">
                    No retained body PDFs yet. Generate message PDFs, then find again.
                  </p>{/if}
              </div>
              {#if attachments !== "omit"}
                <div class="tool-field tool-span-all">
                  <span class="tool-label">Attachment sets</span>
                  {#if view.publications?.total}
                    <p class="tool-muted">Choose one set for each message.</p>
                    {#each publicationGroups(view.publications.items) as group (group.versionID)}
                      <RadioGroup
                        legend={group.legend}
                        name={`publication-${group.versionID}`}
                        value={view.publicationSelections?.[group.versionID]}
                        options={group.options}
                        onchange={(value) => {
                          if (value) controller.selectPublication(group.versionID, value);
                        }}
                      />
                    {/each}
                    <div class="tool-actions tool-actions--start">
                      {#if view.publications.after > 0}<Button
                          size="sm"
                          disabled={busy}
                          onclick={() =>
                            void controller.publicationPage(
                              Math.max(0, view.publications!.after - 50),
                            )}>Previous attachment sets</Button
                        >{/if}
                      {#if view.publications.next > 0}<Button
                          size="sm"
                          disabled={busy}
                          onclick={() => void controller.publicationPage(view.publications!.next)}
                          >Next attachment sets</Button
                        >{/if}
                    </div>
                  {:else if view.publications}<p class="tool-muted">One set per message.</p>
                  {:else}<div class="inline-control">
                      <Button disabled={busy} onclick={() => void controller.publicationPage(0)}
                        >Find attachment sets</Button
                      >
                    </div>{/if}
                  <p class="tool-muted">
                    Only qualified nested-email PDFs become separate attachment PDFs.
                  </p>
                </div>
              {/if}
            {/if}
          </div>
        </details>
      {/if}
      {#if photo && !admitted && !validQuality}<p role="alert" class="tool-error">
          Enter a whole number from 1 to 100 for JPEG quality.
        </p>{/if}
      {#if photo && !admitted && !validLongEdge}<p role="alert" class="tool-error">
          {longEdgeValidationMessage}
        </p>{/if}

      {#if view.error}
        <div class="tool-heading">
          <p role="alert" class="tool-error">
            {view.error.message}
            {#if photo && !admitted && view.error instanceof APIError && view.error.code === "export_role_unavailable"}
              <span
                >{input && "photos" in input && input.photos.asset_ids?.length
                  ? "Remove the unavailable photo from your selection and retry."
                  : "Switch to Export selection and select photos without the unavailable photo."}</span
              >
            {/if}
          </p>
          {#if !admitted}<Button size="sm" onclick={() => controller.resetPreparation()}
              >Start over</Button
            >{/if}
        </div>
      {/if}
      {#if view.status === "expired"}<p role="status">
          This export expired. Choose Download ZIP to start again.
        </p>{/if}

      {#if !admitted && !paused}
        <div class="tool-actions">
          {#if view.status === "preparing"}<span class="tool-muted" role="status"
              ><Spinner size={14} /> {photo ? "Rendering photos…" : "Checking documents…"}</span
            >{/if}
          <Button
            tone="info"
            surface="solid"
            disabled={busy ||
              count === 0 ||
              count > memberLimit ||
              (photo
                ? !validPhotoSettings
                : [original, text, pages, emailPDF].every((value) => value === "omit") ||
                  (emailPDF !== "omit" && !recipe))}
            onclick={() => void downloadZIP()}>Download ZIP</Button
          >
        </div>
      {/if}

      {#if admitted}
        <section class="tool-section" aria-label="Export job" aria-live="polite">
          <div class="tool-heading">
            <h3>
              {view.status === "completed"
                ? "Archive ready"
                : view.status === "disconnected"
                  ? "Disconnected"
                  : view.status === "canceled"
                    ? "Export canceled"
                    : view.status === "failed"
                      ? "Export failed"
                      : view.status === "expired"
                        ? "Export expired"
                        : "Building archive"}
            </h3>
            {#if admitted.job}<span class="tool-muted"
                >{admitted.job.completed_roles.toLocaleString()} of {admitted.plan.role_entries.toLocaleString()}
                files · {formatBytes(admitted.job.completed_bytes)} of {formatBytes(
                  admitted.plan.role_bytes,
                )}</span
              >{/if}
          </div>
          {#if view.status === "disconnected"}<p class="tool-muted">
              The server keeps working. Reconnect to follow it.
            </p>{/if}
          {#if admitted.job?.failure}<p class="tool-error">{admitted.job.failure}</p>{/if}
          {#if receipt}
            <dl class="tool-stats">
              <div>
                <dt>ZIP size</dt>
                <dd
                  data-testid="export-archive-size"
                  data-bytes={receipt.size}
                  title={`${receipt.size.toLocaleString()} bytes`}
                >
                  {formatBytes(receipt.size)}
                </dd>
              </div>
              <div>
                <dt>Entries</dt>
                <dd>{receipt.entries.toLocaleString()}</dd>
              </div>
            </dl>
            <dl class="hashes">
              {@render hash(
                "Archive SHA-256",
                receipt.sha256,
                "export-archive-hash",
                "Copy archive hash",
              )}
            </dl>
            {#if view.downloadOffered}<p class="tool-muted" role="status">
                Download started in your browser.
              </p>{/if}
          {/if}
          <div class="tool-actions">
            {#if !terminal}<Button onclick={() => void controller.cancel()}>Cancel export</Button
              ><Button
                disabled={view.status === "starting"}
                onclick={() => void controller.reconnect()}>Reconnect</Button
              >{/if}
            {#if terminal}<Button
                disabled={view.releasing || view.downloading}
                onclick={() => void prepareAnother()}>New export</Button
              >{/if}
            {#if receipt}<Button
                tone="info"
                surface="solid"
                disabled={view.downloading || view.releasing || view.status === "expired"}
                onclick={() => void controller.download(basename)}
                >{view.downloading ? "Verifying…" : "Download ZIP"}</Button
              >{/if}
          </div>
        </section>
      {/if}

      {#if plan}
        {#if view.problems?.total || view.problemsLoading || view.problemsError}
          <section class="tool-section" aria-label="Unavailable export outputs">
            <div class="tool-heading">
              <h3>Unavailable outputs</h3>
              {#if view.problems}<span class="tool-muted"
                  >{view.problems.after + 1}–{view.problems.after + view.problems.items.length} of
                  {view.problems.total}</span
                >{/if}
            </div>
            {#if view.problems}
              <ul class="tool-list">
                {#each view.problems.items as problem, i (`${view.problems.after}:${i}`)}
                  <li>
                    <div class="tool-row-main">
                      <span>{problem.role.replaceAll("_", " ")}: {problem.reason}</span>
                      <small
                        ><code
                          >{problem.node_id}/{problem.version_id}{problem.part_path
                            ? ` · part ${problem.part_path}`
                            : ""}</code
                        ></small
                      >
                    </div>
                  </li>
                {/each}
              </ul>
              {#if !admitted || terminal}<div class="tool-actions tool-actions--start">
                  {#if view.problems.after > 0}<Button
                      size="sm"
                      disabled={view.problemsLoading}
                      onclick={() =>
                        void controller.problemPage(Math.max(0, view.problems!.after - 50))}
                      >Previous unavailable outputs</Button
                    >{/if}
                  {#if view.problems.next > 0}<Button
                      size="sm"
                      disabled={view.problemsLoading}
                      onclick={() => void controller.problemPage(view.problems!.next)}
                      >Next unavailable outputs</Button
                    >{/if}
                </div>{/if}
            {:else if view.problemsLoading}<p class="tool-muted" role="status">
                <Spinner size={14} /> Loading…
              </p>{/if}
            {#if view.problemsError}
              <div class="tool-heading">
                <p role="alert" class="tool-error">{view.problemsError.message}</p>
                {#if !view.problems && (!admitted || terminal) && view.status !== "expired"}
                  <Button
                    size="sm"
                    disabled={view.problemsLoading}
                    onclick={() => void controller.problemPage(0)}
                    >Retry unavailable output details</Button
                  >
                {/if}
              </div>
            {/if}
          </section>
        {/if}
        {#if paused}
          <div class="tool-actions">
            <Button
              tone="info"
              surface="solid"
              disabled={view.status !== "ready"}
              onclick={() => void continueDownload()}>Download anyway</Button
            >
          </div>
        {/if}
        <details class="tool-disclosure">
          <summary
            ><ChevronRightIcon size="14" aria-hidden="true" /><span class="tool-label">Details</span
            ><span class="tool-disclosure-value"
              >{plan.total.toLocaleString()}
              {plan.photo_render ? "photo" : "document"}{plan.total === 1 ? "" : "s"} · {formatBytes(
                plan.role_bytes,
              )}</span
            ></summary
          >
          <div class="tool-section">
            <dl class="tool-stats">
              <div>
                <dt>{plan.photo_render ? "Photos" : "Documents"}</dt>
                <dd data-testid="export-total">{plan.total.toLocaleString()}</dd>
              </div>
              {#if plan.photo_render}<div>
                  <dt>Format</dt>
                  <dd>
                    {plan.photo_render.format.toUpperCase()} · {plan.photo_render.long_edge ||
                      "Original"} pixels
                  </dd>
                </div>
                <div>
                  <dt>RAW previews</dt>
                  <dd>{plan.embedded_previews ?? 0}</dd>
                </div>{/if}
              <div>
                <dt>Payload</dt>
                <dd>{formatBytes(plan.role_bytes)} · {plan.role_entries.toLocaleString()} files</dd>
              </div>
              {#if plan.counts && !plan.photo_render}
                <div>
                  <dt>Messages</dt>
                  <dd data-testid="export-message-count">
                    {plan.counts.messages.toLocaleString()}
                  </dd>
                </div>
                <div>
                  <dt>Attachments</dt>
                  <dd data-testid="export-attachment-count">
                    {plan.counts.attachments.toLocaleString()}
                  </dd>
                </div>
                <div>
                  <dt>Separate PDFs</dt>
                  <dd>
                    {plan.counts.email_pdfs} bodies · {plan.counts.attachment_pdfs} attachments
                  </dd>
                </div>
                <div>
                  <dt>PDF pages</dt>
                  <dd data-testid="export-page-count">{plan.counts.pages.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>Shared duplicates</dt>
                  <dd>{plan.counts.collapsed.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>Unavailable</dt>
                  <dd>
                    {plan.counts.unavailable} outputs · {plan.counts.unavailable_inventories} inventories
                  </dd>
                </div>
              {/if}
              {#if plan.volume_limits}<div>
                  <dt>ZIP volumes</dt>
                  <dd>{plan.volumes ?? 0} in one download</dd>
                </div>{/if}
              <div>
                <dt>Expires</dt>
                <dd>{formatDate(plan.expires_at)}</dd>
              </div>
            </dl>
            {#if view.reviewed?.plan.id === plan.id}
              <ul class="tool-list">
                {#each view.reviewed.preview.roles as role (role.role)}
                  <li>
                    <div class="tool-row-main">
                      <strong>{role.role.replaceAll("_", " ")}</strong>
                      <span
                        >{role.files} files · {role.collapsed_files ?? 0} shared · {formatBytes(
                          role.bytes,
                        )}{role.unavailable_reason ? ` · ${role.unavailable_reason}` : ""}</span
                      >
                    </div>
                    <Chip size="xs" tone={role.unavailable_members ? "warning" : "success"}
                      >{role.available_members} available · {role.unavailable_members} unavailable</Chip
                    >
                  </li>
                {/each}
              </ul>
            {/if}
            <dl class="hashes">
              {@render hash(
                "Member SHA-256",
                plan.source.member_hash,
                "export-member-hash",
                "Copy export member hash",
              )}
              {@render hash(
                "Plan fingerprint",
                plan.fingerprint,
                "export-plan-fingerprint",
                "Copy export plan fingerprint",
              )}
            </dl>
          </div>
        </details>
      {/if}
    </div>
  </DrawerShell>
{/if}

<style>
  .source {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-4);
    min-width: 0;
  }
  .source strong {
    overflow-wrap: anywhere;
  }
  .checks {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-7);
  }
  .inline-control {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    min-width: 0;
  }
  .inline-control :global(.kit-select-dropdown) {
    flex: 1;
    min-width: 0;
  }
  .hashes {
    display: grid;
    gap: var(--space-4);
    margin: 0;
  }
  .hash {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-2);
    min-width: 0;
  }
  .hash dt {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .hash dd {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
    margin: 0;
  }
  code {
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
    min-width: 0;
  }
</style>
