import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ExportDrawer from "./ExportDrawer.svelte";
beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});
const input = {
  label: "Selected documents",
  members: [
    {
      node_id: 1,
      version_id: "11111111-1111-4111-8111-111111111111",
      sha256: "a".repeat(64),
      size: 12,
    },
  ],
};

it("shows exact source scope and original warning before any server work, with explicit role omission choices", async () => {
  const fetcher = vi.spyOn(globalThis, "fetch");
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Selected documents")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Download ZIP" }).hasAttribute("disabled")).toBe(false);
  expect(screen.queryByRole("button", { name: "Download anyway" })).toBeNull();
  await fireEvent.click(screen.getByRole("combobox", { name: /^Text export policy/ }));
  expect(screen.getByRole("option", { name: "If available" })).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});

it("disables empty sources and fences work when closed", async () => {
  const close = vi.fn();
  render(ExportDrawer, {
    session: "s",
    input: { label: "Empty selection", members: [] },
    open: true,
    onclose: close,
    onauthfailure: vi.fn(),
  });
  expect(
    (await screen.findByRole("button", { name: "Download ZIP" })).hasAttribute("disabled"),
  ).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
  expect(close).toHaveBeenCalledOnce();
});

it("offers body PDFs with explicit partial, attachment, duplicate and bounded packaging choices", async () => {
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("combobox", { name: /^Email body PDF/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Include retained body PDFs" }));
  expect(screen.getByRole("button", { name: "Find retained PDF recipes" })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Email attachment outputs/ })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Partial email export/ }).textContent).toContain(
    "Fail if any output is unavailable",
  );
  expect(screen.getByRole("combobox", { name: /^Duplicate outputs/ }).textContent).toContain(
    "Preserve every occurrence",
  );
  expect(screen.getByRole("button", { name: "Download ZIP" }).hasAttribute("disabled")).toBe(true);
});

it("routes an expired browser session to the app authentication handler", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ detail: "Session ended", code: "unauthorized" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  const onauthfailure = vi.fn(),
    onclose = vi.fn();
  render(ExportDrawer, { session: "s", input, open: true, onclose, onauthfailure });
  await fireEvent.click(await screen.findByRole("button", { name: "Download ZIP" }));
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(onclose).toHaveBeenCalledOnce();
});

it("starts a fresh preparation after a failed source reservation", async () => {
  const ids: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    ids.push(JSON.parse(String(init?.body)).operation_id);
    return new Response(JSON.stringify({ detail: "Source preparation failed" }), { status: 409 });
  });
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Download ZIP" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Start over" }));
  expect(screen.queryByRole("alert")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByRole("button", { name: "Start over" });
  expect(ids).toHaveLength(2);
  expect(new Set(ids).size).toBe(2);
});

it("shows empty attachment sets and sends the set selected in the drawer", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members),
    hash = "a".repeat(64);
  let selected: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url),
      body = init?.body ? JSON.parse(String(init.body)) : undefined;
    const json = (value: unknown) =>
      new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/sources"))
      return json({
        id: body.operation_id,
        request_sha256: hash,
        kind: "explicit",
        state: "sealed",
        member_hash: memberHash,
        total: 1,
        source_bytes: 12,
        created_at: "2026-01-01T00:00:00Z",
        expires_at: "2099-01-01T00:00:00Z",
      });
    const source_id = path.split("/").at(-2);
    if (path.endsWith("/email-pdf-recipes"))
      return json({
        source_id,
        member_hash: memberHash,
        total: 1,
        recipes: [
          { recipe_sha256: hash, paper: "A4", renderer_version: "151", messages: 1, ambiguous: 0 },
        ],
      });
    if (path.includes("/attachment-publications"))
      return json({
        source_id,
        member_hash: memberHash,
        after: 0,
        next: 0,
        total: 2,
        items: ["first", "second"].map((operation_id) => ({
          node_id: 1,
          version_id: input.members[0]!.version_id,
          name: "empty.eml",
          operation_id,
          generation_id: hash,
          created_at: "2026-01-01T00:00:00Z",
          state: "complete",
          attachments: 0,
        })),
      });
    selected = body.publications;
    return new Response(JSON.stringify({ detail: "No retained PDF" }), { status: 409 });
  });
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  const choose = async (name: RegExp, option: string | RegExp) => {
    await fireEvent.click(screen.getByRole("combobox", { name }));
    await fireEvent.click(screen.getByRole("option", { name: option }));
  };
  await choose(/^Email body PDF/, "Include retained body PDFs");
  await fireEvent.click(screen.getByRole("button", { name: "Find retained PDF recipes" }));
  await screen.findByRole("combobox", { name: /^Retained PDF recipe/ });
  await choose(/^Retained PDF recipe/, /A4/);
  await choose(/^Email attachment outputs/, "Include original attachments");
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  const second = await screen.findByRole("radio", { name: /second/ });
  expect(screen.getAllByRole("radio").every((r) => !(r as HTMLInputElement).checked)).toBe(true);
  await fireEvent.click(second);
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await waitFor(() =>
    expect(selected).toEqual([
      { version_id: input.members[0]!.version_id, operation_id: "second" },
    ]),
  );
});

it("keeps export start available when the first details page fails and retries that page", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members),
    hash = "a".repeat(64),
    future = "2099-01-01T00:00:00Z";
  let source: any,
    plan: any,
    detailRequests = 0,
    planRequests = 0;
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url),
      body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path.endsWith("/sources")) {
      source = {
        id: body.operation_id,
        request_sha256: hash,
        kind: "explicit",
        state: "sealed",
        member_hash: memberHash,
        total: 1,
        source_bytes: 12,
        created_at: "2026-01-01T00:00:00Z",
        expires_at: future,
      };
      return json(source);
    }
    if (path.endsWith("/plans")) {
      planRequests++;
      plan = {
        format: "docbank-bundle-v1",
        id: body.operation_id,
        vault_id: input.members[0]!.version_id,
        toolchain: "go1.27",
        source,
        roles: body.roles,
        fingerprint: hash,
        total: 1,
        role_entries: 1,
        role_bytes: 12,
        metadata_bytes: 100,
        created_at: source.created_at,
        expires_at: future,
        counts: {
          messages: 0,
          attachments: 0,
          email_pdfs: 0,
          attachment_pdfs: 0,
          pages: 0,
          collapsed: 0,
          unavailable: 1,
          unavailable_inventories: 0,
        },
      };
      return json(plan);
    }
    if (path.endsWith("/preview"))
      return json({
        plan_id: plan.id,
        fingerprint: hash,
        member_hash: memberHash,
        total: 1,
        roles: [
          { role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 },
          {
            role: "text",
            available_members: 0,
            unavailable_members: 1,
            files: 0,
            bytes: 0,
            unavailable_reason: "No retained text",
          },
        ],
      });
    expect(path).toBe(`/api/v1/exports/plans/${plan.id}/problems?after=0`);
    if (++detailRequests === 1)
      return new Response(JSON.stringify({ detail: "Details temporarily unavailable" }), {
        status: 500,
      });
    return json({
      plan_id: plan.id,
      fingerprint: hash,
      after: 0,
      next: 0,
      total: 1,
      items: [
        {
          node_id: 1,
          version_id: input.members[0]!.version_id,
          role: "text",
          reason: "Text was not retained for this version.",
        },
      ],
    });
  });
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("combobox", { name: /^Text export policy/ }));
  await fireEvent.click(screen.getByRole("option", { name: "If available" }));
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByText("Details temporarily unavailable");
  const start = screen.getByRole("button", { name: "Download anyway" });
  expect(start.hasAttribute("disabled")).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Retry unavailable output details" }));
  await screen.findByText("text: Text was not retained for this version.");
  expect(screen.queryByRole("alert")).toBeNull();
  expect(start.hasAttribute("disabled")).toBe(false);
  expect(detailRequests).toBe(2);
  expect(planRequests).toBe(1);
});

it("validates photo settings, drops document options, and scopes failure recovery", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members);
  let requested: any,
    failureCode = "export_limit";
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/sources"))
      return Response.json({
        id: body.operation_id,
        request_sha256: "a".repeat(64),
        kind: "photos",
        state: "sealed",
        member_hash: memberHash,
        total: 1,
        source_bytes: 12,
        created_at: "2026-01-01T00:00:00Z",
        expires_at: "2099-01-01T00:00:00Z",
      });
    requested = body;
    return new Response(JSON.stringify({ detail: "Captured preparation", code: failureCode }), {
      status: failureCode === "export_limit" ? 413 : 422,
    });
  });
  const view = render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("combobox", { name: /^ZIP packaging/ }));
  await fireEvent.click(screen.getByRole("option", { name: "One output / 512 MiB per volume" }));
  await fireEvent.click(screen.getByRole("combobox", { name: /^Duplicate outputs/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Share exact duplicate outputs" }));
  const { photoQuery } = await import("./photos.svelte.js");
  const photos = {
    label: "Selected photos",
    total: 1,
    photos: { hidden: false, query: photoQuery },
  };
  await view.rerender({ input: photos });
  expect(screen.queryByRole("combobox", { name: /^ZIP packaging/ })).toBeNull();
  const edge = screen.getByRole("textbox", { name: "Long edge, pixels" });
  const prepare = screen.getByRole("button", { name: "Download ZIP" });
  for (const value of ["100001", "-1", "text", "0", "1.5"]) {
    await fireEvent.input(edge, { target: { value } });
    expect(prepare.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("alert").textContent).toBe(
      "Enter a whole number from 1 to 100,000 pixels, or leave Long edge blank.",
    );
  }
  for (const value of ["1", "100000", ""]) {
    await fireEvent.input(edge, { target: { value } });
    expect(prepare.hasAttribute("disabled")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  }
  const quality = screen.getByRole("textbox", { name: "JPEG quality" });
  for (const value of ["", "0", "101", "text", "1.5"]) {
    await fireEvent.input(quality, { target: { value } });
    expect(prepare.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("alert").textContent).toBe(
      "Enter a whole number from 1 to 100 for JPEG quality.",
    );
  }
  for (const value of ["1", "100", "90"]) {
    await fireEvent.input(quality, { target: { value } });
    expect(prepare.hasAttribute("disabled")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  }
  await fireEvent.click(screen.getByRole("checkbox", { name: "Include image metadata" }));
  const gps = screen.getByRole("checkbox", { name: "Remove GPS" }) as HTMLInputElement;
  expect(gps.disabled).toBe(true);
  expect(gps.checked).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByText("Captured preparation");
  expect(requested.photo_render).toEqual({
    format: "jpeg",
    quality: 90,
    long_edge: 0,
    include_metadata: false,
    remove_gps: false,
  });
  expect(requested.volume_limits).toBeUndefined();
  expect(requested.duplicate_policy).toBeUndefined();
  expect(screen.queryByText(/unavailable photo/)).toBeNull();
  failureCode = "export_role_unavailable";
  await fireEvent.click(screen.getByRole("button", { name: "Start over" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Include image metadata" }));
  const restoredGPS = screen.getByRole("checkbox", { name: "Remove GPS" }) as HTMLInputElement;
  expect(restoredGPS.disabled).toBe(false);
  expect(restoredGPS.checked).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByText(
    "Switch to Export selection and select photos without the unavailable photo.",
  );
  expect(requested.photo_render).toMatchObject({ include_metadata: true, remove_gps: true });
  await fireEvent.click(screen.getByRole("button", { name: "Start over" }));
  await view.rerender({
    input: { ...photos, photos: { ...photos.photos, asset_ids: [input.members[0]!.version_id] } },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByText("Remove the unavailable photo from your selection and retry.");
});

it("captures current album members on reopen while retaining settings retries and admitted jobs", async () => {
  const albumID = "22222222-2222-4222-8222-222222222222",
    future = "2099-01-01T00:00:00Z";
  const removedPhoto = {
    asset_id: "33333333-3333-4333-8333-333333333333",
    member_hash: "a".repeat(64),
  };
  const replacementPhoto = {
    asset_id: "44444444-4444-4444-8444-444444444444",
    member_hash: "b".repeat(64),
  };
  let albumMembers = [removedPhoto];
  const album = {
    label: "Synthetic album",
    total: 1,
    photos: {
      query: {
        v: 1,
        syntax: "advanced",
        mode: "lexical",
        text: "",
        filters: { set_ids: [albumID] },
        sort: { field: "added_time", direction: "desc" },
      },
      asset_ids: [],
      hidden: false,
    },
  };
  const sources = new Map<string, { source: any; members: string[] }>();
  const plans: any[] = [],
    jobs: any[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url),
      body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path.endsWith("/sources")) {
      expect(body).toMatchObject({
        kind: "photos",
        photos: { query: { filters: { set_ids: [albumID] } }, asset_ids: [], hidden: false },
      });
      if (!sources.has(body.operation_id))
        sources.set(body.operation_id, {
          source: {
            id: body.operation_id,
            request_sha256: "c".repeat(64),
            kind: "photos",
            state: "sealed",
            member_hash: albumMembers[0]!.member_hash,
            total: albumMembers.length,
            source_bytes: 12,
            created_at: "2026-01-01T00:00:00Z",
            expires_at: future,
          },
          members: albumMembers.map((photo) => photo.asset_id),
        });
      return Response.json(sources.get(body.operation_id)!.source);
    }
    if (path.endsWith("/plans")) {
      const source = sources.get(body.source_id)!.source;
      expect(body.member_hash).toBe(source.member_hash);
      const plan = {
        format: "docbank-bundle-v1",
        id: body.operation_id,
        vault_id: albumID,
        toolchain: "go1.27",
        source,
        roles: body.roles,
        photo_render: body.photo_render,
        fingerprint: "c".repeat(64),
        total: 1,
        role_entries: 1,
        role_bytes: 12,
        metadata_bytes: 100,
        created_at: source.created_at,
        expires_at: future,
      };
      plans.push(plan);
      return Response.json(plan);
    }
    const plan = plans.at(-1);
    if (path.endsWith("/preview"))
      return Response.json({
        plan_id: plan.id,
        fingerprint: plan.fingerprint,
        member_hash: plan.source.member_hash,
        total: 1,
        roles: [
          {
            role: "photo_rendered",
            available_members: 1,
            unavailable_members: 0,
            files: 1,
            bytes: 12,
          },
        ],
      });
    if (init?.method === "DELETE") return new Response(null, { status: 204 });
    expect(path).toBe("/api/v1/exports/jobs");
    jobs.push(body);
    return Response.json({
      id: body.operation_id,
      plan_id: plan.id,
      fingerprint: plan.fingerprint,
      state: "canceled",
      sequence: 1,
      completed_roles: 0,
      completed_bytes: 0,
      attempt: 1,
      created_at: plan.created_at,
      deadline: future,
      expires_at: future,
    });
  });
  const view = render(ExportDrawer, {
    session: "s",
    input: album,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.input(screen.getByRole("textbox", { name: "JPEG quality" }), {
    target: { value: "80" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await screen.findByRole("button", { name: "New export" });
  expect(screen.getByTestId("export-member-hash").textContent).toBe("a".repeat(64));
  expect(plans.at(-1).photo_render.quality).toBe(80);
  expect(sources.size).toBe(1);
  expect(jobs).toHaveLength(1);

  await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
  await view.rerender({ open: false });
  albumMembers = [replacementPhoto];
  await view.rerender({ open: true, input: structuredClone(album) });
  await screen.findByRole("button", { name: "New export" });
  expect(screen.getByTestId("export-member-hash").textContent).toBe("a".repeat(64));
  expect(sources.size).toBe(1);

  await fireEvent.click(screen.getByRole("button", { name: "New export" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Download ZIP" }));
  await waitFor(() => expect(jobs).toHaveLength(2));
  await screen.findByRole("button", { name: "New export" });
  expect(screen.getByTestId("export-member-hash").textContent).toBe("b".repeat(64));
  expect(sources.size).toBe(2);
  expect(sources.get(plans.at(-1).source.id)!.members).toEqual([
    "44444444-4444-4444-8444-444444444444",
  ]);
  expect(plans.at(-1).photo_render.quality).toBe(80);
});

it.each([false, true])(
  "releases a document export and handles a retained request: %s",
  async (retainedRequest) => {
    const { exportMemberHash } = await import("./exports.js");
    const memberHash = await exportMemberHash(input.members),
      hash = "a".repeat(64),
      future = "2099-01-01T00:00:00Z";
    let source: any, plan: any;
    const plans: any[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      const path = String(url),
        body = init?.body ? JSON.parse(String(init.body)) : undefined;
      if (path.endsWith("/sources")) {
        source = {
          id: body.operation_id,
          request_sha256: hash,
          kind: "explicit",
          state: "sealed",
          member_hash: memberHash,
          total: 1,
          source_bytes: 12,
          created_at: "2026-01-01T00:00:00Z",
          expires_at: future,
        };
        return Response.json(source);
      }
      if (path.endsWith("/plans")) {
        plans.push(body);
        plan = {
          format: "docbank-bundle-v1",
          id: body.operation_id,
          vault_id: input.members[0]!.version_id,
          toolchain: "go1.27",
          source,
          roles: body.roles,
          duplicate_policy: body.duplicate_policy,
          fingerprint: hash,
          total: 1,
          role_entries: 1,
          role_bytes: 12,
          metadata_bytes: 100,
          created_at: source.created_at,
          expires_at: future,
        };
        return Response.json(plan);
      }
      if (path.endsWith("/preview"))
        return Response.json({
          plan_id: plan.id,
          fingerprint: hash,
          member_hash: memberHash,
          total: 1,
          roles: [
            { role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 },
          ],
        });
      if (path.endsWith("/jobs"))
        return Response.json({
          id: body.operation_id,
          plan_id: plan.id,
          fingerprint: hash,
          state: "canceled",
          sequence: 1,
          completed_roles: 0,
          completed_bytes: 0,
          attempt: 1,
          created_at: source.created_at,
          deadline: future,
          expires_at: future,
        });
      expect(init?.method).toBe("DELETE");
      return new Response(null, { status: 204 });
    });
    const onprepareanother = vi.fn(),
      onclose = vi.fn();
    const view = render(ExportDrawer, {
      session: "s",
      input,
      open: true,
      onclose,
      onauthfailure: vi.fn(),
      onprepareanother,
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "ZIP filename" }), {
      target: { value: "documents.zip" },
    });
    await fireEvent.click(screen.getByRole("combobox", { name: /^Duplicate outputs/ }));
    await fireEvent.click(screen.getByRole("option", { name: "Share exact duplicate outputs" }));
    await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
    await screen.findByRole("button", { name: "New export" });
    if (retainedRequest) await view.rerender({ notice: "Your previous export is still retained." });
    await fireEvent.click(screen.getByRole("button", { name: "New export" }));
    await screen.findByRole("button", { name: "Download ZIP" });
    expect(onprepareanother).toHaveBeenCalledTimes(Number(retainedRequest));
    if (retainedRequest) return;
    expect(onclose).not.toHaveBeenCalled();
    expect((screen.getByRole("textbox", { name: "ZIP filename" }) as HTMLInputElement).value).toBe(
      "documents.zip",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
    await screen.findByRole("button", { name: "New export" });
    expect(plans).toHaveLength(2);
    expect(plans[1].roles).toEqual(plans[0].roles);
    expect(plans[1].duplicate_policy).toBe("collapse_exact_content");
  },
);

it("builds and offers the ZIP from one Download ZIP click when every output is available", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members),
    hash = "a".repeat(64),
    future = "2099-01-01T00:00:00Z";
  const receipt = {
    format: "docbank-bundle-v1",
    plan_fingerprint: hash,
    sha256: "b".repeat(64),
    size: 120,
    entries: 4,
  };
  const ticketURL = `/api/daemon/web-download/file?ticket=${"t".repeat(43)}`;
  let source: any, plan: any;
  const paths: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url),
      body = init?.body ? JSON.parse(String(init.body)) : undefined;
    paths.push(path);
    if (path.endsWith("/sources")) {
      source = {
        id: body.operation_id,
        request_sha256: hash,
        kind: "explicit",
        state: "sealed",
        member_hash: memberHash,
        total: 1,
        source_bytes: 12,
        created_at: "2026-01-01T00:00:00Z",
        expires_at: future,
      };
      return Response.json(source);
    }
    if (path.endsWith("/plans")) {
      plan = {
        format: "docbank-bundle-v1",
        id: body.operation_id,
        vault_id: input.members[0]!.version_id,
        toolchain: "go1.27",
        source,
        roles: body.roles,
        fingerprint: hash,
        total: 1,
        role_entries: 1,
        role_bytes: 12,
        metadata_bytes: 100,
        created_at: source.created_at,
        expires_at: future,
      };
      return Response.json(plan);
    }
    if (path.endsWith("/preview"))
      return Response.json({
        plan_id: plan.id,
        fingerprint: hash,
        member_hash: memberHash,
        total: 1,
        roles: [
          { role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 },
        ],
      });
    if (path.endsWith("/download")) return Response.json({ url: ticketURL, receipt });
    if (path.endsWith("/jobs"))
      return Response.json({
        id: body.operation_id,
        plan_id: plan.id,
        fingerprint: hash,
        state: "completed",
        sequence: 1,
        completed_roles: 1,
        completed_bytes: 12,
        attempt: 1,
        receipt,
        created_at: source.created_at,
        deadline: future,
        expires_at: future,
      });
    throw new Error(`unexpected request ${path}`);
  });
  const clicked: string[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    clicked.push(this.getAttribute("href") ?? "");
  });
  render(ExportDrawer, {
    session: "s",
    input,
    open: true,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(screen.getByRole("button", { name: "Download ZIP" }));
  await waitFor(() => expect(clicked).toEqual([ticketURL]));
  expect(screen.queryByRole("button", { name: "Download anyway" })).toBeNull();
  expect(paths.filter((path) => path.endsWith("/download"))).toHaveLength(1);
  expect(await screen.findByText("Download started in your browser.")).toBeTruthy();
});
