import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import type { DatePage, Identity, Node, Plan, Summary } from "../src/generated/docbank.js";
import { getGetTermReportUrl } from "../src/generated/docbank.js";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const captures = process.env.DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR;
test.skip(!captures, "DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR enables this qualification");

test("qualifies selected reports and original exports", async ({ page }) => {
  test.setTimeout(240_000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY;
  if (!binary || !path.isAbsolute(binary)) {
    throw new Error("DOCBANK_SCREENSHOT_BINARY must name an absolute branch binary");
  }
  await access(binary);
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-report-export-"));
  const vault = path.join(scratch, "vault");
  const run = async (...args: string[]) => (await exec(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 90_000,
  })).stdout.trim();
  const sha256 = (bytes: Buffer) => createHash("sha256").update(bytes).digest("hex");
  const history = async () => JSON.parse(await run("search-export", "history", "--json"));
  const post = (route: string) => page.waitForResponse(response =>
    new URL(response.url()).pathname === route && response.request().method() === "POST");
  let started = false;
  const stop = async () => {
    if (!started) return;
    let pid: number | undefined;
    try {
      const before = JSON.parse(await run("daemon", "status", "--json"));
      pid = before.pid;
      if (before.running) await run("daemon", "stop");
      const after = JSON.parse(await run("daemon", "status", "--json"));
      if (after.running !== false) throw new Error("Daemon is still running");
    } catch {
      throw new Error(`Synthetic daemon ${pid ?? "unknown"} did not stop; retained ${scratch}`);
    }
  };
  try {
    await mkdir(captures!, { recursive: true, mode: 0o700 });
    const sources = new Map([
      ["one.txt", Buffer.from("alpha. Document dated 2024-05-06.\n")],
      ["two.txt", Buffer.from("beta. Document dated 2024-05-06. Document dated 2024-06-07.\n")],
      ["missing.bin", Buffer.from([0x00, 0xff, 0x00, 0xff, 0x55, 0xaa])],
      ["excluded.txt", Buffer.from("alpha beta. Document dated 2024-05-06.\n")],
    ]);
    const nodes: Node[] = [];
    for (const [name, bytes] of sources) {
      const source = path.join(scratch, name);
      await writeFile(source, bytes, { mode: 0o600 });
      started = true; // add may start the daemon even if the command fails.
      await run("add", source, "--dest", "/");
      const node: Node = JSON.parse(await run("stat", `/${name}`, "--json"));
      expect(node.blob_hash).toBe(sha256(bytes));
      expect(node.size).toBe(bytes.length);
      nodes.push(node);
    }
    const [one, two, missing, excluded] = nodes as [Node, Node, Node, Node];
    expect(missing.mime_type).toBe("application/octet-stream");
    for (const [term, hit] of [["alpha", one], ["beta", two]] as const) {
      await expect.poll(async () => {
        const result = JSON.parse(await run("search", term, "--json"));
        return result.hits.map((row: { node: Node }) => row.node.id)
          .sort((a: number, b: number) => a - b);
      }, { timeout: 30_000 }).toEqual([hit.id, excluded.id].sort((a, b) => a - b));
    }
    const identities: Identity[] = nodes.slice(0, 3).map(node => ({
      node_id: node.id, version_id: node.current_version_id!, sha256: node.blob_hash!,
    }));
    const members = identities.map((id, i) => ({ ...id, size: nodes[i]!.size }));
    const issued = new URL(await run("web", "--no-browser"));
    const session = new URLSearchParams(issued.hash.slice(1)).get("web_session");
    if (!session) throw new Error("Synthetic web session missing");
    try { await page.goto(issued.toString()); }
    catch { throw new Error("Could not open the synthetic browser session"); }
    const drawer = page.getByRole("dialog", { name: "Search exports", exact: true });
    const exportDrawer = page.getByRole("dialog", { name: "Verified export", exact: true });

    await test.step("strict missing-text refusal leaves history unchanged", async () => {
      const before = await history();
      await page.getByRole("checkbox", { name: "Select missing.bin", exact: true }).check();
      await page.getByRole("button", { name: "Report selected documents", exact: true }).click();
      await drawer.getByRole("textbox", { name: "Expression for term 1" }).fill("alpha");
      await expect(drawer.getByRole("combobox", { name: "Coverage policy" })).toHaveValue("strict");
      const creating = post("/api/v1/search-exports");
      await drawer.getByRole("button", { name: "Create export", exact: true }).click();
      const response = await creating;
      expect(response.status()).toBe(422);
      expect(await response.json()).toMatchObject({ code: "incomplete_coverage" });
      await expect(drawer.getByRole("alert")).toBeVisible();
      await expect(drawer.getByRole("table")).toHaveCount(0);
      await expect(drawer.getByRole("button", { name: "Download evidence ZIP" })).toHaveCount(0);
      expect(await history()).toEqual(before);
      await drawer.getByRole("button", { name: "Close search exports" }).click();
      await page.getByRole("checkbox", { name: "Select missing.bin", exact: true }).uncheck();
    });

    const archive = path.join(scratch, "originals.zip");
    let fingerprint = "";
    await test.step("export exactly the three originals through the browser", async () => {
      for (const node of nodes.slice(0, 3)) {
        await page.getByRole("checkbox", { name: `Select ${node.name}`, exact: true }).check();
      }
      await expect(page.getByRole("checkbox", { name: "Select excluded.txt" })).not.toBeChecked();
      await page.getByRole("button", { name: "Export selection", exact: true }).click();
      const sourcing = post("/api/v1/exports/sources");
      const planning = post("/api/v1/exports/plans");
      await exportDrawer.getByRole("button", { name: "Preview export", exact: true }).click();
      const source = await sourcing;
      expect(source.status()).toBe(200);
      const submittedSource = source.request().postDataJSON();
      expect(submittedSource.kind).toBe("explicit");
      expect(submittedSource.members.toSorted((a: Identity, b: Identity) => a.node_id - b.node_id))
        .toEqual(members);
      const response = await planning;
      expect(response.status()).toBe(200);
      const plan: Plan = await response.json();
      expect(plan).toMatchObject({ total: 3, role_entries: 3, roles: [{ role: "original" }] });
      expect(plan.roles).toHaveLength(1);
      expect(plan.volume_limits).toBeUndefined();
      expect(plan.duplicate_policy ?? "preserve").toBe("preserve");
      fingerprint = plan.fingerprint;
      await expect(exportDrawer.getByTestId("export-plan-fingerprint")).toHaveText(fingerprint);
      await exportDrawer.getByRole("button", { name: "Start reviewed export" }).click();
      await expect(exportDrawer.getByText("Archive verified and ready", { exact: true }))
        .toBeVisible({ timeout: 30_000 });
      await exportDrawer.getByRole("button", { name: "Download verified ZIP" })
        .scrollIntoViewIfNeeded();
      await page.screenshot({
        path: path.join(captures!, "export-ready.png"), animations: "disabled",
      });
      const hash = await exportDrawer.getByTestId("export-archive-hash").innerText();
      const size = Number(await exportDrawer.getByTestId("export-archive-size")
        .getAttribute("data-bytes"));
      const downloading = page.waitForEvent("download");
      await exportDrawer.getByRole("button", { name: "Download verified ZIP" }).click();
      const download = await downloading;
      await download.saveAs(archive);
      expect(await download.failure()).toBeNull();
      const bytes = await readFile(archive);
      expect(bytes.length).toBe(size);
      expect(sha256(bytes)).toBe(hash);
      await exportDrawer.getByRole("button", { name: "Close export" }).click();
    });

    let child: Summary;
    let reviewedChoice: unknown;
    await test.step("review only the ambiguous document and preserve the parent", async () => {
      for (const node of nodes.slice(0, 3)) {
        await page.getByRole("checkbox", { name: `Select ${node.name}`, exact: true }).check();
      }
      await page.getByRole("button", { name: "Report selected documents", exact: true }).click();
      await drawer.getByRole("textbox", { name: "Expression for term 1" }).fill("alpha");
      await drawer.getByRole("button", { name: "Add term", exact: true }).click();
      await drawer.getByRole("textbox", { name: "Expression for term 2" }).fill("beta");
      await drawer.getByRole("combobox", { name: "Coverage policy" })
        .selectOption("available_only");
      const creating = post("/api/v1/search-exports");
      await drawer.getByRole("button", { name: "Create export", exact: true }).click();
      const response = await creating;
      expect(response.status()).toBe(200);
      const submitted = response.request().postDataJSON();
      expect(submitted.selected_documents.documents
        .toSorted((a: Identity, b: Identity) => a.node_id - b.node_id)).toEqual(identities);
      expect(submitted.all_documents).toBe(false);
      expect(submitted.collection_ids ?? []).toEqual([]);
      expect(submitted.timezone).toBe("UTC");
      for (const term of submitted.terms) {
        expect(term.dates.start).toBe("2000-01-01");
        expect(term.dates.end >= "2024-06-07").toBe(true);
        expect(term.dates.end >= missing.created_at.slice(0, 10)).toBe(true);
      }
      const parent: Summary = await response.json();
      expect(parent).toMatchObject({ state: "needs_review", unresolved_dates: 1 });
      expect(parent.counts ?? []).toEqual([]);
      await expect(drawer.getByRole("table")).toHaveCount(0);
      await expect(drawer.getByRole("button", { name: "Download evidence ZIP" })).toHaveCount(0);
      const loading = page.waitForResponse(r =>
        new URL(r.url()).pathname === `/api/v1/search-exports/${parent.id}/dates`);
      await drawer.getByRole("button", { name: "Review dates", exact: true }).click();
      const dates: DatePage = await (await loading).json();
      const member = dates.members.find(member => member.document.node_id === two.id)!;
      expect(member.selection.date).toBe("");
      const candidate = member.candidates.find(candidate =>
        candidate.source_class === "content" && candidate.value === "2024-05-06")!;
      expect(candidate).toBeDefined();
      const review = drawer.locator(".review-item").filter({
        has: page.getByText(`Document ${two.id}`, { exact: true }),
      });
      await review.getByRole("radio", { name: "document_date · 2024-05-06 · content" }).check();
      await review.getByRole("textbox", { name: "Reason", exact: true })
        .fill("Use the first stated document date.");
      reviewedChoice = {
        action: "select", candidate_id: candidate.id, document: identities[1],
        evidence_sha256: candidate.locator.evidence_sha256,
        reason: "Use the first stated document date.",
      };
      await review.scrollIntoViewIfNeeded();
      await page.screenshot({
        path: path.join(captures!, "date-review.png"), animations: "disabled",
      });
      const revising = post(`/api/v1/search-exports/${parent.id}/revisions`);
      await drawer.getByRole("button", { name: "Create reviewed revision" }).click();
      const revision = await revising;
      expect(revision.status()).toBe(200);
      expect(revision.request().postDataJSON()).toEqual({ choices: [reviewedChoice] });
      child = await revision.json();
      expect(child!.id).not.toBe(parent.id);
      expect(child!).toMatchObject({
        state: "complete", parent_id: parent.id, unresolved_dates: 0,
      });
      const parentRead = await page.evaluate(async ({ url, session }) => {
        const response = await fetch(url, {
          headers: { "X-Docbank-Web-Session": session },
        });
        return { status: response.status, summary: await response.json() };
      }, { url: getGetTermReportUrl(parent.id), session });
      expect(parentRead).toMatchObject({ status: 200, summary: { state: "needs_review" } });
      const counts = { hits: 1, hits_plus_family: 1, unique_hits: 1,
        unique_families: 1, unique_hits_plus_family: 1 };
      const coverage = { scoped: 3, searchable: 2, missing_text: 1,
        incomplete_families: 0, fallback_dates: 1 };
      expect(child!.counts).toEqual([counts, counts]);
      expect(child!.coverage).toMatchObject(coverage);
      expect(child!.row_coverage).toHaveLength(2);
      for (const row of child!.row_coverage!) expect(row).toMatchObject(coverage);
      const table = drawer.getByRole("table", { name: "Search export counts" });
      for (const [index, term] of ["alpha", "beta"].entries()) {
        const cells = table.getByRole("row").nth(index + 1).locator("span");
        expect(await cells.allTextContents()).toEqual([
          String(index + 1), term,
          `${submitted.terms[index].dates.start} to ${submitted.terms[index].dates.end}`,
          "1", "1", "1", "1", "1",
        ]);
      }
      await expect(drawer.getByText(
        "3 scoped · 2 searchable · 1 missing text · 0 incomplete families · 1 fallback dates",
        { exact: true },
      )).toBeVisible();
      await table.scrollIntoViewIfNeeded();
      await page.screenshot({
        path: path.join(captures!, "report-counts.png"), animations: "disabled",
      });
    });

    const packet = path.join(scratch, "report.zip");
    const browserCSV = path.join(scratch, "browser.csv");
    for (const [label, destination] of [
      ["Download evidence ZIP", packet], ["Download CSV", browserCSV],
    ]) {
      const downloading = page.waitForEvent("download");
      await drawer.getByRole("button", { name: label!, exact: true }).click();
      const download = await downloading;
      await download.saveAs(destination!);
      expect(await download.failure()).toBeNull();
    }
    expect(await run("search-export", "verify", packet)).toContain("internally consistent: true");
    const extractedCSV = path.join(scratch, "extracted.csv");
    await run("search-export", "csv", packet, "--output", extractedCSV);

    // Independent readers use fixture bytes and literal outcomes, never the report calculator.
    const evidence = path.join(scratch, "expected.json");
    await writeFile(evidence, JSON.stringify({ identities, fingerprint, reviewedChoice,
      missingDate: missing.created_at.slice(0, 10), terms: child!.terms,
      originals: nodes.slice(0, 3).map(node => ({ ...members.find(m => m.node_id === node.id),
        hex: sources.get(node.name)!.toString("hex") })),
    }), { mode: 0o600 });
    const verifyArtifacts = async () => exec("python3", ["-c", String.raw`
import csv, hashlib, io, json, sys, zipfile
from pathlib import Path
expected = json.loads(Path(sys.argv[1]).read_text())
identities = expected['identities']
counts = dict(hits=1, hits_plus_family=1, unique_hits=1,
              unique_families=1, unique_hits_plus_family=1)
coverage = dict(scoped=3, searchable=2, missing_text=1,
                incomplete_families=0, fallback_dates=1)
with zipfile.ZipFile(sys.argv[2]) as z:
    assert sorted(z.namelist()) == sorted([
        'manifest.json', 'members.jsonl', 'families.jsonl', 'dates.jsonl', 'hits.csv'])
    manifest = json.loads(z.read('manifest.json'))
    members = [json.loads(line) for line in z.read('members.jsonl').splitlines()]
    dates = [json.loads(line) for line in z.read('dates.jsonl').splitlines()]
    assert [m['identity'] for m in members] == identities
    assert [d['document'] for d in dates] == identities
    # Empty family IDs designate unrelated singletons, keyed by their identities.
    assert [m['family_id'] for m in members] == ['', '', '']
    assert all(m['coverage']['family_state'] == 'complete' for m in members)
    assert z.read('families.jsonl') == b''
    assert manifest['counts'] == [counts, counts]
    assert len(manifest['row_coverage']) == 2
    for actual in [manifest['coverage'], *manifest['row_coverage']]:
        assert {key: actual[key] for key in coverage} == coverage
    assert [m['coverage']['search_state'] for m in members] == ['complete', 'complete', 'missing']
    assert [m['hits'] for m in members] == [[True, False], [False, True], [False, False]]
    assert [m['selection']['date'] for m in members] == [
        '2024-05-06', '2024-05-06', expected['missingDate']]
    assert [m['selection']['reason'] for m in members] == [
        'content', 'Use the first stated document date.', 'vault_addition']
    assert dates[1]['choice'] == expected['reviewedChoice']
    for member, date in zip(members, dates):
        candidate_id = member['selection']['candidate_id']
        selected = next(c for c in date['candidates'] if c['id'] == candidate_id)
        assert selected['source_class'] == ('vault_addition' if member is members[2] else 'content')
    rows = list(csv.reader(io.StringIO(z.read('hits.csv').decode())))
    literal = [['Term #', 'Terms', 'Date Range', 'Hits', 'Hits Plus Family',
                'Unique Hits', 'Unique Families', 'Unique Hits Plus Family']]
    for i, term in enumerate(['alpha', 'beta']):
        cutoff = expected['terms'][i]['dates']
        literal.append([str(i+1), term, cutoff['start']+' to '+cutoff['end'], *(['1']*5)])
    assert rows == literal
    for filename in sys.argv[3:5]:
        with open(filename, newline='') as f:
            assert list(csv.reader(f)) == literal
with zipfile.ZipFile(sys.argv[5]) as z:
    manifest = json.loads(z.read('bundle.json'))
    plan, docs = manifest['plan'], manifest['documents']
    assert [dict(node_id=d['node_id'], version_id=d['version_id'], sha256=d['sha256'])
            for d in docs] == identities
    paths = []
    for doc, original in zip(docs, expected['originals']):
        assert len(doc['roles']) == 1
        role = doc['roles'][0]
        assert role['role'] == 'original' and role['status'] == 'available'
        raw = z.read(role['path'])
        assert raw == bytes.fromhex(original['hex'])
        assert len(raw) == original['size'] == role['size'] == doc['size']
        assert hashlib.sha256(raw).hexdigest() == original['sha256'] == role['sha256']
        paths.append(role['path'])
    assert sorted(z.namelist()) == sorted(paths + ['bundle.json', 'metadata.csv', 'SHA256SUMS'])
    checksums = dict((line[66:], line[:64]) for line in z.read('SHA256SUMS').decode().splitlines())
    assert set(checksums) == set(z.namelist()) - {'SHA256SUMS'}
    for name, digest in checksums.items():
        assert hashlib.sha256(z.read(name)).hexdigest() == digest
    metadata = list(csv.DictReader(io.StringIO(z.read('metadata.csv').decode())))
    assert len(metadata) == 3
    assert [dict(node_id=int(row['node_id']), version_id=row['version_id'],
                 sha256=row['source_sha256']) for row in metadata] == identities
    assert all(row['role'] == 'original' and row['status'] == 'available' for row in metadata)
    member_hash = hashlib.sha256(''.join(
        str(i['node_id'])+':'+i['version_id']+'\n' for i in identities).encode()).hexdigest()
    assert plan['source']['member_hash'] == member_hash
    fingerprint = plan['fingerprint']
    assert fingerprint == expected['fingerprint']
    plan['fingerprint'] = ''
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'),
                                        ensure_ascii=False).encode()
    digest = hashlib.sha256(canonical(plan)+b'\n')
    for doc in docs:
        digest.update(canonical(doc)+b'\n')
    assert digest.hexdigest() == fingerprint
print('Verified report evidence, CSV and all three original byte streams')
`, evidence, packet, browserCSV, extractedCSV, archive], { timeout: 30_000 });
    await verifyArtifacts();

    await test.step("frozen downloads survive replacement but a stale rerun is refused", async () => {
      await writeFile(path.join(scratch, "one.txt"),
        "Replacement content. Document dated 2024-05-06.\n", { mode: 0o600 });
      await run("add", path.join(scratch, "one.txt"), "--dest", "/", "--replace");
      const replaced: Node = JSON.parse(await run("stat", "/one.txt", "--json"));
      expect(replaced.current_version_id).not.toBe(one.current_version_id);
      expect(replaced.blob_hash).not.toBe(one.blob_hash);
      const downloading = page.waitForEvent("download");
      await drawer.getByRole("button", { name: "Download evidence ZIP", exact: true })
        .click();
      const download = await downloading;
      const later = path.join(scratch, "after-replacement.zip");
      await download.saveAs(later);
      expect(await download.failure()).toBeNull();
      expect(await readFile(later)).toEqual(await readFile(packet));
      const before = await history();
      expect(before.items.map((item: { summary: Summary }) => item.summary.id))
        .toContain(child!.id);
      const completed = drawer.locator(".history-item").filter({ hasText: "Counts ready" });
      await completed.getByRole("button", { name: "Use as draft" }).click();
      const creating = post("/api/v1/search-exports");
      await drawer.getByRole("button", { name: "Create export", exact: true }).click();
      const response = await creating;
      expect(response.request().postDataJSON().selected_documents.documents)
        .toEqual(identities);
      expect(response.status()).toBe(409);
      expect(await response.json()).toMatchObject({ code: "report_selection_changed" });
      await expect(drawer.getByRole("alert")).toHaveText(
        "Selected documents changed. Refresh the workspace and reselect the intended current versions.",
      );
      await expect(drawer.getByRole("table")).toHaveCount(0);
      expect(await history()).toEqual(before);
    });

    await stop();
    expect(await run("search-export", "verify", packet)).toContain("internally consistent: true");
    await verifyArtifacts();
    expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
    await exec("python3", ["-c", String.raw`
import sqlite3, sys
from pathlib import Path
uri = Path(sys.argv[1]).resolve().as_uri() + '?mode=ro&immutable=1'
connection = sqlite3.connect(uri, uri=True)
try:
    assert connection.execute('SELECT COUNT(*) FROM rendition_jobs').fetchone() == (0,)
    assert connection.execute('SELECT COUNT(*) FROM embedding_jobs').fetchone() == (0,)
finally:
    connection.close()
`, path.join(vault, "docbank.db")], { timeout: 30_000 });
  } finally {
    await stop();
    await rm(scratch, { recursive: true, force: true });
  }
});
