<script lang="ts">
  import {
    Button,
    Checkbox,
    SelectDropdown,
    Spinner,
    Notice,
    Table,
    TableHeaderCell,
  } from "@kenn-io/kit-ui";
  import TagPicker from "./TagPicker.svelte";
  import type { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
  import type {
    PhotoAuthoredPatch,
    PhotoKeywordEdit,
    PhotoAuthoredReceipt,
    Tag,
  } from "./generated/docbank.js";
  let {
    editor,
    count = 1,
    actionDisabled = false,
    onedit,
    onresponse,
  }: {
    editor: PhotoAuthoredEditor;
    count?: number;
    actionDisabled?: boolean;
    onresponse?: (receipt?: PhotoAuthoredReceipt, tags?: Tag[]) => Promise<void>;
    onedit: (patch: PhotoAuthoredPatch, keywords?: PhotoKeywordEdit[]) => void;
  } = $props();
  let keyword = $state("");
  let selections = $state<Record<string, { caption: boolean; tags: string[] }>>({});
  function toggleSuggestion(id: string, tag?: string) {
    const selected = selections[id] ?? { caption: false, tags: [] };
    selections[id] =
      tag === undefined
        ? { ...selected, caption: !selected.caption }
        : {
            ...selected,
            tags: selected.tags.includes(tag)
              ? selected.tags.filter((value) => value !== tag)
              : [...selected.tags, tag],
          };
  }
  const disabled = $derived(
    actionDisabled ||
      editor.concealed ||
      editor.conflict ||
      editor.busy ||
      editor.loading ||
      (!!editor.error && !editor.targets.length),
  );
  const fields = ["caption", "creator", "copyright"] as const;
  // Each original owns its sidecar status.
  const xmpTargets = $derived(
    editor.files.filter(
      (target) =>
        target.xmp?.state !== "none" &&
        editor.asset?.files.some(
          (file) => file.id === target.file_id && (file.role === "raw" || file.role === "image"),
        ),
    ),
  );
  const xmpLabels: Record<string, string> = {
    synced: "Synced",
    pending: "Pending",
    conflict: "Conflict",
    unavailable: "Unavailable",
    invalid: "Invalid",
  };
  const choices = [
    {
      field: "rating",
      label: "Rating",
      options: [0, 1, 2, 3, 4, 5].map((value) => ({
        value: String(value),
        label: value ? `${value} star${value === 1 ? "" : "s"}` : "Unrated",
      })),
    },
    {
      field: "flag",
      label: "Flag",
      options: [
        { value: "", label: "Unflagged" },
        { value: "pick", label: "Pick" },
        { value: "reject", label: "Reject" },
      ],
    },
    {
      field: "label",
      label: "Color label",
      options: ["", "red", "yellow", "green", "blue", "purple"].map((value) => ({
        value,
        label: value ? value[0]!.toUpperCase() + value.slice(1) : "No label",
      })),
    },
    {
      field: "rotation",
      label: "Rotation",
      options: [0, 90, 180, 270].map((value) => ({ value: String(value), label: `${value}°` })),
    },
  ] as const;
  function select(field: "rating" | "flag" | "label" | "rotation", value: string) {
    if (value === "mixed") return;
    editor.stage({ [field]: field === "rating" || field === "rotation" ? Number(value) : value });
  }
</script>

<section class="photo-inspector" aria-label="Photo metadata">
  <h2>{count === 1 ? "Metadata" : `${count.toLocaleString()} photos`}</h2>
  {#if editor.concealed}{#if editor.validating}<span role="status"
        ><Spinner size={14} />Loading metadata…</span
      >{:else}<span role="status">Metadata unavailable</span>{/if}{:else}
    {#if editor.loading}<span role="status"><Spinner size={14} />Loading metadata…</span>{/if}
    {#if editor.asset && editor.asset.files.filter((file) => file.role !== "sidecar").length > 1}
      <SelectDropdown
        title="Edit members"
        value={editor.fileID}
        options={[
          { value: "", label: "Whole pair" },
          ...editor.asset.files
            .filter((file) => file.role !== "sidecar")
            .map((file) => ({
              value: file.id,
              label: `${file.role.toUpperCase()} · ${editor.targets.find((target) => target.file_id === file.id)?.file_name ?? file.id}`,
            })),
        ]}
        disabled={disabled || editor.dirty}
        onchange={(value) => editor.member(value)}
      />
    {/if}
    {#if count === 1 && editor.activeSuggestion}
      {@const suggestion = editor.activeSuggestion}
      {@const source = editor.targets.find((target) => target.file_id === suggestion.target)}
      {@const member = editor.asset?.files.find((file) => file.id === suggestion.target)}
      {@const selected = selections[suggestion.suggestion_id]}
      {@const caption = !!suggestion.value.caption && !!selected?.caption}
      {@const tags = suggestion.value.tags.filter((tag) => selected?.tags.includes(tag))}
      {@const replaces = !!source?.values.caption}
      {@const replacementID = `caption-replacement-${suggestion.suggestion_id}`}
      <section class="suggestions" aria-label="Suggested photo metadata">
        <header class="suggestion-heading">
          <h3>Suggested</h3>
          {#if source && editor.asset && editor.asset.files.filter((file) => file.role !== "sidecar").length > 1}
            <p class="suggestion-target" title={source.file_name}>
              {#if member}<span class="suggestion-role">{member.role.toUpperCase()} ·</span>{/if}
              <span class="suggestion-filename">{source.file_name}</span>
            </p>
          {/if}
        </header>
        {#if suggestion.value.caption}
          <div class="suggestion-field">
            <h4>Caption</h4>
            <Checkbox
              class="suggested-caption"
              label={suggestion.value.caption}
              checked={caption}
              ariaDescribedby={replaces ? replacementID : undefined}
              disabled={disabled || editor.dirty}
              onchange={() => toggleSuggestion(suggestion.suggestion_id)}
            />
            {#if replaces}<p id={replacementID} class="suggestion-note">
                Replaces current caption
              </p>{/if}
          </div>
        {/if}
        {#if suggestion.value.tags.length}
          <fieldset class="suggestion-field suggested-keywords">
            <legend>Keywords</legend>
            <div class="suggested-tags">
              {#each suggestion.value.tags as tag (tag)}
                <Checkbox
                  class="suggested-keyword"
                  label={tag}
                  checked={tags.includes(tag)}
                  disabled={disabled || editor.dirty}
                  onchange={() => toggleSuggestion(suggestion.suggestion_id, tag)}
                />
              {/each}
            </div>
          </fieldset>
        {/if}
        {#if suggestion.value.visible_text}
          <div class="suggestion-field">
            <h4>Visible text</h4>
            <p class="visible-text">{suggestion.value.visible_text}</p>
          </div>
        {/if}
        {#if editor.dirty}<p class="suggestion-note">Apply or clear your edits first.</p>{/if}
        {#if editor.suggestionsError}<Notice
            tone="warning"
            message={editor.suggestionsError}
          />{/if}
        <div class="suggestion-actions">
          {#if suggestion.value.caption || suggestion.value.tags.length}
            <Button
              size="sm"
              tone="info"
              surface="solid"
              disabled={disabled || editor.dirty || (!caption && !tags.length)}
              onclick={() =>
                void editor.respondSuggestion(suggestion, caption, tags, false, onresponse)}
              >Accept selected</Button
            >
          {/if}
          <Button
            size="sm"
            disabled={disabled || editor.dirty}
            onclick={() => void editor.respondSuggestion(suggestion, false, [], true, onresponse)}
            >Dismiss</Button
          >
        </div>
      </section>
    {:else if editor.suggestionsError}<Notice
        tone="warning"
        message={editor.suggestionsError}
      />{/if}
    {#each choices as choice}
      {@const value = editor.value(choice.field)}
      <div class="field">
        <span>{choice.label}</span><SelectDropdown
          title={choice.label}
          value={value === undefined ? "mixed" : String(value)}
          options={[
            ...(value === undefined ? [{ value: "mixed", label: "Mixed" }] : []),
            ...choice.options,
          ]}
          {disabled}
          onchange={(value) => select(choice.field, value)}
        />
      </div>
    {/each}
    {#each fields as field}
      {@const value = editor.value(field)}
      <div class="field">
        <span>{field[0]!.toUpperCase() + field.slice(1)}</span><textarea
          aria-label={field[0]!.toUpperCase() + field.slice(1)}
          value={String(value ?? "")}
          placeholder={value === undefined ? "Mixed" : ""}
          {disabled}
          rows="2"
          oninput={(event) => editor.stage({ [field]: event.currentTarget.value })}
        ></textarea>{#if value === undefined}<Button
            size="sm"
            {disabled}
            onclick={() => editor.stage({ [field]: "" })}>Clear {field}</Button
          >{/if}
      </div>
    {/each}
    {#if editor.tagsError}<Notice tone="error" message={editor.tagsError} /><Button
        size="sm"
        disabled={editor.tagsLoading}
        onclick={() => void editor.loadTags?.()}>Retry keywords</Button
      >{/if}
    {#if editor.tagsTotal > editor.tags.length}<Button
        size="sm"
        disabled={editor.tagsLoading}
        onclick={() => void editor.loadMoreTags?.()}>Load more keywords</Button
      >{/if}
    <div class="field">
      <span>Keywords</span><TagPicker
        value={keyword}
        tags={editor.tags}
        title="Keyword"
        placeholder="Choose keyword"
        disabled={disabled || editor.tagsLoading}
        onchange={(value) => (keyword = value)}
      />
    </div>
    {#if keyword}<div class="keyword-actions">
        <Button
          size="sm"
          {disabled}
          onclick={() => editor.stage({}, [{ tag_id: keyword, assign: true }])}>Add keyword</Button
        ><Button
          size="sm"
          {disabled}
          onclick={() => editor.stage({}, [{ tag_id: keyword, assign: false }])}
          >Remove keyword</Button
        >{#if editor.keywordState(keyword)}<span
            >{editor.keywordState(keyword) === "mixed" ? "Mixed" : "Assigned"}</span
          >{/if}
      </div>{/if}
    {#if editor.files.length}<ul aria-label="Assigned keywords">
        {#each editor.tags.filter((tag) => editor.keywordState(tag.id)) as tag}<li>
            {tag.name}{editor.keywordState(tag.id) === "mixed" ? " · Mixed" : ""}
          </li>{/each}
      </ul>{/if}
    {#each xmpTargets as target}
      {@const xmp = target.xmp}
      <section class="xmp-state" aria-label={`XMP for ${target.file_name}`}>
        <h3>{target.file_name}</h3>
        <div role="status">
          {#if xmp}<span>XMP: {xmpLabels[xmp.state]}</span>{/if}
          {#if xmp?.reason}<p>{xmp.reason}</p>{/if}
        </div>
        {#if xmp?.state === "pending" && xmp.reason}<Button
            size="sm"
            {disabled}
            onclick={() => void editor.refreshXMP()}>Refresh XMP status</Button
          >{/if}
        {#if xmp?.conflict_id}
          <Table ariaLabel="XMP conflicts" bare unsorted>
            <thead
              ><tr>
                <TableHeaderCell label="Field" /><TableHeaderCell label="Catalog" /><TableHeaderCell
                  label="Sidecar"
                />
              </tr></thead
            ><tbody>
              {#each xmp.rows as row}<tr
                  ><td>{row.field}</td><td>{row.catalog || "Empty"}</td><td
                    >{row.sidecar || "Empty"}</td
                  ></tr
                >{/each}
            </tbody>
          </Table>
          <div class="keyword-actions">
            <Button
              size="sm"
              disabled={disabled || editor.dirty || xmp.state !== "conflict"}
              onclick={() => void editor.resolveXMP(target.file_id, "catalog")}>Use Catalog</Button
            ><Button
              size="sm"
              disabled={disabled || editor.dirty || xmp.state !== "conflict"}
              onclick={() => void editor.resolveXMP(target.file_id, "sidecar")}>Use Sidecar</Button
            >
          </div>
        {/if}
        {#if xmp?.import_receipt_id}<Button
            size="sm"
            disabled={disabled || editor.dirty}
            onclick={() => void editor.undoSidecarImport(target.file_id)}
            >Undo sidecar import</Button
          >{/if}
      </section>
    {/each}
    {#if editor.conflict && (!editor.membershipChanged || !editor.targets.length)}<Notice
        tone="warning"
        message={editor.targets.length
          ? "Review the current photos before applying these edits."
          : "No photos are available to review. Clear edits to discard this draft."}
      />{/if}
    {#if editor.membershipChanged && editor.targets.length}
      <Notice
        tone="warning"
        message="Photo membership changed. These remaining files will receive your edits."
      />
      <ul aria-label="Members to review">
        {#each editor.files as target}<li>{target.file_name}</li>{/each}
      </ul>
      <Button disabled={editor.busy || editor.loading} onclick={() => editor.reviewMembers()}
        >Use reviewed members</Button
      >
    {:else if editor.conflict && editor.targets.length}<Button
        disabled={editor.busy || editor.loading}
        onclick={() => void editor.reload()}>Reload metadata</Button
      >{/if}
    <Button disabled={disabled || editor.conflict || !editor.dirty} onclick={() => onedit({})}
      >Apply</Button
    >
  {/if}
  {#if editor.dirty}<Button
      disabled={editor.busy || editor.loading}
      onclick={() => editor.clearDraft()}>Clear edits</Button
    >{/if}
</section>

<style>
  .suggestions {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding: var(--space-5);
    background: var(--bg-inset);
    border-radius: var(--radius-md);
    min-width: 0;
  }
  .suggestions h3,
  .suggestions h4,
  .suggestions p {
    margin: 0;
  }
  .suggestions h3 {
    font-size: var(--font-size-sm);
    font-weight: 600;
  }
  .suggestion-heading {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .suggestion-target {
    display: flex;
    gap: var(--space-2);
    min-width: 0;
  }
  .suggestion-role {
    flex-shrink: 0;
  }
  .suggestion-filename {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .suggestion-target,
  .suggestion-note,
  .suggestions h4,
  .suggested-keywords legend {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    font-weight: 400;
  }
  .suggestion-field {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
  }
  .suggestions :global(.suggested-caption) {
    align-items: flex-start;
    gap: var(--space-4);
  }
  .suggestions :global(.suggested-caption .kit-checkbox__box) {
    margin-top: var(--space-1);
  }
  .suggestions :global(.suggested-caption .kit-checkbox__label) {
    font-size: var(--font-size-sm);
    line-height: 1.45;
    color: var(--text-primary);
    overflow-wrap: anywhere;
  }
  .suggested-keywords {
    border: 0;
    padding: 0;
    margin: 0;
  }
  .suggested-keywords legend {
    padding: 0;
    margin-bottom: var(--space-2);
  }
  .suggested-tags,
  .suggestion-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .suggestions :global(.suggested-keyword) {
    gap: var(--space-2);
    min-height: 28px;
    box-sizing: border-box;
    max-width: 100%;
    padding: var(--space-2) var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: 999px;
    background: var(--bg-surface);
    overflow-wrap: anywhere;
  }
  .suggestions :global(.suggested-keyword:has(input:checked)) {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface));
  }
  .suggestions :global(.suggested-keyword:has(input:checked) .kit-checkbox__label) {
    color: var(--text-primary);
    font-weight: 600;
  }
  .suggestions .visible-text {
    padding: var(--space-3) var(--space-4);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    font-size: var(--font-size-xs);
    line-height: 1.45;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .suggestion-actions {
    padding-top: var(--space-2);
  }
  @media (pointer: coarse) {
    .suggestions :global(.suggested-keyword) {
      min-height: 32px;
    }
  }
  .photo-inspector {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  h2 {
    margin: 0;
    font-size: var(--font-size-sm);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: var(--font-size-xs);
  }
  .field > span {
    color: var(--text-muted);
  }
  textarea {
    width: 100%;
    box-sizing: border-box;
    resize: vertical;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
  }
  .keyword-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
    font-size: var(--font-size-xs);
  }
  ul {
    margin: 0;
    padding-left: 16px;
    font-size: var(--font-size-xs);
  }
  .xmp-state {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--font-size-xs);
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }
  :global(.xmp-state table) {
    width: 100%;
    border-collapse: collapse;
    table-layout: fixed;
  }
  :global(.xmp-state th),
  :global(.xmp-state td) {
    padding: 4px;
    text-align: left;
    vertical-align: top;
    overflow-wrap: anywhere;
  }
</style>
