import { createHash, randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const output = process.env.DOCBANK_EXPORT_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_EXPORT_SCREENSHOT_DIR enables the synthetic export proof");
interface Member { node_id: number; content_version_id: string; blob_hash: string; size: number; path: string }
interface Snapshot { snapshot_id: string; member_hash: string; total: number; rows: Member[]; next_cursor?: string }

test("downloads the exact 1001-member frozen source through the real export worker", async ({ page }) => {
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-export-proof-"));
  const vault = path.join(workspace, "vault"), sources = path.join(workspace, "Export proof");
  const run = async (...args: string[]) => (await exec(path.join(repository, "docbank"), args, { cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 300_000, maxBuffer: 4 * 1024 * 1024 })).stdout.trim();
  try {
    await mkdir(sources, { mode: 0o700 });
    for (let i = 0; i < 1001; i++) await writeFile(path.join(sources, `export-${String(i).padStart(4, "0")}.txt`), `Synthetic export document ${i}. Original frozen bytes.\n`, { mode: 0o600 });
    await run("add", sources, "--dest", "/");
    await run("tag", "create", "Changed after capture");
    const rawURL = await run("web", "--no-browser"), url = new URL(rawURL);
    const fragment = new URLSearchParams(url.hash.slice(1));
    const session = fragment.get("web_session");
    if (!session) throw new Error("Synthetic web session missing");
    const query = { v: 1, text: "", syntax: "simple", mode: "lexical", filters: { no_tags: true }, sort: { field: "path", direction: "asc" } };
    fragment.set("query", JSON.stringify(query)); url.hash = fragment.toString();
    const api = async <T>(route: string, body?: unknown): Promise<T> => {
      const response = await fetch(`http://127.0.0.1:${url.port}${route}`, { method: body === undefined ? "GET" : "POST", signal: AbortSignal.timeout(60_000), headers: { Host: url.host, "User-Agent": "OpenAI File Downloader, XaiImageApiFetch/1.0", "X-Docbank-Web-Session": session, "Content-Type": "application/json" }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) });
      if (!response.ok) throw new Error(`Synthetic request failed ${route}: ${response.status}`);
      return response.json() as Promise<T>;
    };
    page.setDefaultTimeout(45_000);
    page.on("pageerror", error => console.error(`Browser error: ${error.message}`));
    await page.setViewportSize({ width: 1440, height: 1200 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(url.toString());
    const acceptedPromise = page.waitForResponse(r => new URL(r.url()).pathname === "/api/v1/workspace/queries" && r.request().method() === "POST");
    await page.getByRole("button", { name: "Run query", exact: true }).click();
    const accepted = await (await acceptedPromise).json() as Snapshot;
    expect(accepted.total).toBe(1001);
    const frozen = [...accepted.rows];
    let cursor = accepted.next_cursor;
    while (cursor) {
      const next = await api<Snapshot>(`/api/v1/workspace/queries/${accepted.snapshot_id}/pages`, { cursor });
      frozen.push(...next.rows); cursor = next.next_cursor;
    }
    expect(frozen).toHaveLength(1001);
    await page.getByRole("button", { name: "Close query editor", exact: true }).click();

    // Replace a captured head, remove that row from the live query, and add a
    // different matching document. Equal live counts must not hide substitution.
    const replacement = path.join(workspace, "replacement.txt");
    await writeFile(replacement, "Synthetic newer head; never part of the frozen export.\n", { mode: 0o600 });
    await run("put", replacement, frozen[0]!.path);
    const current = await api<{ revision: number }>(`/api/v1/nodes/${frozen[0]!.node_id}`);
    const tags = await api<{ items: { id: string; name: string }[] }>("/api/v1/tags?limit=1000&offset=0");
    await api("/api/v1/batch/tags", { operation_id: randomUUID(), tag_id: tags.items.find(t => t.name === "Changed after capture")!.id, assign: true, nodes: [{ node_id: frozen[0]!.node_id, revision: current.revision }] });
    const later = path.join(workspace, "export-later.txt");
    await writeFile(later, "Synthetic document added after snapshot capture.\n", { mode: 0o600 });
    await run("add", later, "--dest", "/Export proof");
    const live = await api<Snapshot>("/api/v1/workspace/queries", { query, page_size: 100, facets: [] });
    expect(live.total).toBe(1001);
    expect(live.member_hash).not.toBe(accepted.member_hash);

    const chunks: number[] = [];
    page.on("request", request => {
      if (/\/exports\/sources\/[^/]+\/chunks\/\d+$/.test(new URL(request.url()).pathname)) chunks.push((request.postDataJSON() as { members: unknown[] }).members.length);
    });
    await page.getByRole("button", { name: "Export", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Verified export" });
    await expect(drawer.getByText("Whole frozen query", { exact: true })).toBeVisible();
    for (const role of ["Text", "Pages"]) {
      await drawer.getByRole("combobox", { name: new RegExp(`^${role} export policy`) }).click();
      await page.getByRole("option", { name: "Optional — allow unavailable", exact: true }).click();
    }
    await drawer.getByLabel("Downloaded ZIP filename", { exact: true }).fill("Synthetic frozen review.zip");
    await drawer.getByRole("button", { name: "Preview export", exact: true }).click();
    await expect(drawer.getByRole("button", { name: "Start reviewed export", exact: true })).toBeEnabled({ timeout: 180_000 });
    expect(chunks).toEqual([1000, 1]);
    await expect(drawer.getByTestId("export-member-hash")).toHaveText(accepted.member_hash);
    const fingerprint = await drawer.getByTestId("export-plan-fingerprint").innerText();
    await expect(drawer.getByText("No eligible retained text in the frozen plan.", { exact: true })).toBeVisible();
    await expect(drawer.getByText("No complete retained page recipe in the frozen plan.", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Start reviewed export", exact: true }).click();
    await expect(drawer.getByText("Archive verified and ready", { exact: true })).toBeVisible({ timeout: 180_000 });
    await mkdir(output!, { recursive: true, mode: 0o700 });
    await page.screenshot({ path: path.join(output!, "web-export-ready.png"), animations: "disabled" });
    const shownHash = await drawer.getByTestId("export-archive-hash").innerText();
    const shownSize = Number((await drawer.getByTestId("export-archive-size").innerText()).replace(" bytes", ""));
    const downloading = page.waitForEvent("download");
    await drawer.getByRole("button", { name: "Download verified ZIP", exact: true }).click();
    const download = await downloading;
    expect(download.suggestedFilename()).toBe("Synthetic frozen review.zip");
    const archive = path.join(output!, "synthetic-frozen-review.zip");
    await download.saveAs(archive);
    expect(await download.failure()).toBeNull();
    const bytes = await readFile(archive);
    expect(bytes.length).toBe(shownSize);
    expect(createHash("sha256").update(bytes).digest("hex")).toBe(shownHash);

    // Python's independent ZIP/CSV/JSON readers verify the actual browser
    // download, including every checksum and the canonical plan fingerprint.
    const verified = JSON.parse((await exec("python3", ["-c", String.raw`
import csv, hashlib, io, json, pathlib, sys, zipfile
archive = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(archive) as z:
    names = z.namelist()
    assert len(names) == len(set(names))
    assert all(not n.startswith('/') and '..' not in n.split('/') and '\\' not in n for n in names)
    manifest = json.loads(z.read('bundle.json'))
    p, docs = manifest['plan'], manifest['documents']
    checksums = dict((line[66:], line[:64]) for line in z.read('SHA256SUMS').decode().splitlines())
    assert set(checksums) == set(names) - {'SHA256SUMS'}
    for name, digest in checksums.items():
        assert hashlib.sha256(z.read(name)).hexdigest() == digest
    members, role_files, role_bytes = [], [], 0
    for d in docs:
        members.append([d['node_id'], d['version_id'], d['sha256'], d['size']])
        assert [r['role'] for r in d['roles']] == ['original', 'text', 'pages']
        for r in d['roles']:
            if r['role'] == 'original':
                assert r['status'] == 'available'
                b = z.read(r['path'])
                assert len(b) == r['size'] == d['size']
                assert hashlib.sha256(b).hexdigest() == r['sha256'] == d['sha256']
                role_files.append(r['path']); role_bytes += len(b)
            else:
                assert r['status'] == 'unavailable' and not r.get('path')
    assert set(names) == set(role_files) | {'bundle.json', 'metadata.csv', 'SHA256SUMS'}
    rows = list(csv.DictReader(io.StringIO(z.read('metadata.csv').decode())))
    assert len(rows) == len(docs) * 3
    member_hash = hashlib.sha256(''.join(str(m[0])+':'+m[1]+'\n' for m in sorted(members)).encode()).hexdigest()
    assert member_hash == p['source']['member_hash']
    fingerprint = p['fingerprint']; p['fingerprint'] = ''
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()
    h = hashlib.sha256(canonical(p)+b'\n')
    for d in docs: h.update(canonical(d)+b'\n')
    assert h.hexdigest() == fingerprint
    assert len(docs) == p['total'] == p['source']['total']
    assert len(role_files) == p['role_entries'] and role_bytes == p['role_bytes']
    print(json.dumps(dict(total=len(docs), member_hash=member_hash, fingerprint=fingerprint, entries=len(names), role_bytes=role_bytes, members=sorted(members))))
`, archive], { maxBuffer: 4 * 1024 * 1024 })).stdout) as { total: number; member_hash: string; fingerprint: string; entries: number; role_bytes: number; members: [number, string, string, number][] };
    expect(verified.total).toBe(1001);
    expect(verified.member_hash).toBe(accepted.member_hash);
    expect(verified.fingerprint).toBe(fingerprint);
    expect(verified.members).toEqual(frozen.map(m => [m.node_id, m.content_version_id, m.blob_hash, m.size] as [number, string, string, number]).sort((a, b) => a[0] - b[0] || a[1].localeCompare(b[1])));
    const { members: _members, ...summary } = verified;
    await writeFile(path.join(output!, "export-proof.json"), JSON.stringify({ ...summary, archive_sha256: shownHash, archive_size: shownSize, chunk_sizes: chunks, live_member_hash: live.member_hash, frozen_head_preserved: true }, null, 2) + "\n", { mode: 0o600 });
    await expect(drawer.getByText("Download handed to your browser. Check its download list for completion.", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Close export", exact: true }).click();
    await page.getByRole("button", { name: "Export", exact: true }).click();
    await expect(drawer.getByText("Archive verified and ready", { exact: true })).toBeVisible();
    console.log(`Verified ${verified.total} frozen documents, ${verified.entries} ZIP entries, chunks ${chunks.join("+")}; final hash ${shownHash}`);
  } finally {
    const status = JSON.parse(await run("daemon", "status", "--json")) as { running: boolean };
    if (status.running) await run("daemon", "stop");
    expect((JSON.parse(await run("daemon", "status", "--json")) as { running: boolean }).running).toBe(false);
    await rm(workspace, { recursive: true, force: true });
    await expect(access(workspace)).rejects.toThrow();
  }
});
