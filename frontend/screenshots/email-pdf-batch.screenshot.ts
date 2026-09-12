import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { crc32 } from "node:zlib";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const output = process.env.DOCBANK_EMAILPDF_BATCH_PROOF;
const bundle = process.env.DOCBANK_EMAILPDF_TEST_BUNDLE;
const fonts = process.env.DOCBANK_EMAILPDF_TEST_FONTS;
const digest = (bytes: Buffer) => createHash("sha256").update(bytes).digest("hex");
// Reuse the explicitly supplied qualification runtime; no separate browser
// installation or developer browser profile is needed on the retained runner.
test.use({ launchOptions: bundle ? { executablePath: path.join(bundle, "chrome") } : {} });

interface Identity { node_id: number; version_id: string; sha256: string; size: number }
interface Occurrence { target: Identity }
interface Relation { filename: string; child: Identity; order: number }

// An independent stored-ZIP fixture: no Docbank archive writer is used to
// construct the input that its Takeout importer must verify.
function takeout(source: Buffer): Buffer {
  const name = Buffer.from("Takeout/Mail/All mail.mbox");
  const local = Buffer.alloc(30), central = Buffer.alloc(46), end = Buffer.alloc(22);
  local.writeUInt32LE(0x04034b50); local.writeUInt16LE(20, 4);
  local.writeUInt32LE(crc32(source), 14); local.writeUInt32LE(source.length, 18);
  local.writeUInt32LE(source.length, 22); local.writeUInt16LE(name.length, 26);
  central.writeUInt32LE(0x02014b50); central.writeUInt16LE(20, 4); central.writeUInt16LE(20, 6);
  central.writeUInt32LE(crc32(source), 16); central.writeUInt32LE(source.length, 20);
  central.writeUInt32LE(source.length, 24); central.writeUInt16LE(name.length, 28);
  end.writeUInt32LE(0x06054b50); end.writeUInt16LE(1, 8); end.writeUInt16LE(1, 10);
  end.writeUInt32LE(central.length + name.length, 12);
  end.writeUInt32LE(local.length + name.length + source.length, 16);
  return Buffer.concat([local, name, source, central, name, end]);
}

test("real Takeout exports verified PDFs and explicit attachment occurrences through the browser", async ({ page }) => {
  test.skip(!output || !bundle || !fonts, "requires the explicit synthetic pinned-renderer proof environment");
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-email-batch-proof-"));
  const vault = path.join(workspace, "vault");
  const run = async (...args: string[]) => (await exec(path.join(repository, "docbank"), args, {
    cwd: repository,
    env: { PATH: process.env.PATH, LANG: "C.UTF-8", DOCBANK_HOME: vault },
    timeout: 180_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  let started = false;
  try {
    await mkdir(vault, { mode: 0o700 });
    await mkdir(output!, { recursive: true, mode: 0o700 });
    const pin = async (directory: string) => (await exec("bash", ["scripts/email-pdf-pins.sh", directory], { cwd: repository })).stdout.trim();
    const config = `[email_pdf]\nchromium=${JSON.stringify(path.join(bundle!, "chrome"))}\nbundle=${JSON.stringify(bundle)}\nbundle_sha256=${JSON.stringify(await pin(bundle!))}\nversion="151.0.7922.34"\nfonts=${JSON.stringify(fonts)}\nfonts_sha256=${JSON.stringify(await pin(fonts!))}\n`;
    await writeFile(path.join(vault, "config.toml"), config, { mode: 0o600 });
    const nested = "Subject: Synthetic nested message\nFrom: child@example.test\nContent-Type: text/plain; charset=utf-8\n\nIndependently rendered nested message body.\n";
    const message = "From sender@example.test Sat Sep 12 10:00:00 2026\n" +
      "Subject: Same synthetic subject\nMessage-ID: <duplicate@example.test>\n" +
      "X-Gmail-Labels: Inbox,Export proof\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=outer\n\n" +
      "--outer\nContent-Type: text/plain; charset=utf-8\n\nComplete synthetic parent body.\n> Full original quoted body.\n" +
      "--outer\nContent-Type: message/rfc822\nContent-Disposition: attachment; filename=nested.eml\n\n" + nested +
      "\n--outer\nContent-Type: text/plain\nContent-Disposition: attachment; filename=unsupported.txt\n\nOriginal unsupported child bytes.\n--outer--\n";
    const zipPath = path.join(workspace, "Synthetic Takeout.zip");
    await writeFile(zipPath, takeout(Buffer.from(message + message)), { mode: 0o600 });
    started = true;
    const url = new URL(await run("web", "--no-browser"));
    expect(url.protocol).toBe("http:");
    expect(url.hostname).toMatch(/^docbank-[0-9a-f]{32}\.localhost$/);
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session");
    if (!session) throw new Error("missing synthetic browser session");
    const api = async <T>(route: string): Promise<T> => {
      const response = await fetch(`http://127.0.0.1:${url.port}${route}`, {
        signal: AbortSignal.timeout(60_000),
        headers: { Host: url.host, "X-Docbank-Web-Session": session, "User-Agent": "OpenAI File Downloader, XaiImageApiFetch/1.0" },
      });
      if (!response.ok) throw new Error(`synthetic API ${route}: ${response.status}`);
      return response.json() as Promise<T>;
    };
    page.setDefaultTimeout(45_000);
    await page.goto(url.toString(), { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Import mailbox", exact: true }).click();
    await page.getByLabel("Choose MBOX or Takeout ZIP", { exact: true }).setInputFiles(zipPath);
    await page.getByRole("button", { name: "Upload and preview", exact: true }).click();
    await expect(page.getByText("1 mailbox entry · mboxrd")).toBeVisible();
    await page.getByRole("button", { name: "Import messages", exact: true }).click();
    await expect(page.getByText("Import complete", { exact: true })).toBeVisible({ timeout: 180_000 });
    const identity = await page.locator(".mailbox small").textContent();
    if (!identity?.startsWith("Import ")) throw new Error("missing completed import identity");
    const importID = identity.slice(7);
    const occurrences = JSON.parse(await run("mailbox", "receipts", importID)) as Occurrence[];
    expect(occurrences).toHaveLength(2);
    expect(new Set(occurrences.map(o => o.target.node_id)).size).toBe(2);
    expect(new Set(occurrences.map(o => o.target.sha256)).size).toBe(1);
    const rendered: { version: string; sha256: string; kind: string }[] = [];
    const expectedMembers = occurrences.map(o => o.target);
    const render = async (identity: Identity, kind: "parent" | "nested") => {
      const pdf = path.join(workspace, `${identity.version_id}.pdf`);
      await run("email-pdf", identity.version_id, pdf);
      const text = (await exec("pdftotext", [pdf, "-"])).stdout;
      expect(text).toContain(kind === "parent" ? "Complete synthetic parent body" : "Independently rendered nested message body");
      if (kind === "parent") expect(text).toContain("Full original quoted body");
      const bytes = await readFile(pdf);
      rendered.push({ version: identity.version_id, sha256: digest(bytes), kind });
    };
    for (const occurrence of occurrences) {
      await render(occurrence.target, "parent");
      const publication = await api<{ total: number; items: { relation: Relation }[] }>(`/api/v1/email-document-relations?parent_version_id=${occurrence.target.version_id}&limit=100`);
      expect(publication.total).toBe(2);
      const relations = publication.items.map(item => item.relation);
      expect(relations).toHaveLength(2);
      expect(relations.map(r => r.filename).sort()).toEqual(["nested.eml", "unsupported.txt"]);
      expectedMembers.push(...relations.map(r => r.child));
      await render(relations.find(r => r.filename === "nested.eml")!.child, "nested");
    }
    expect(rendered).toHaveLength(4);
    expect(new Set(rendered.map(r => r.version)).size).toBe(4);
    const indexed = JSON.parse(await run("search", "Independently", "--json")) as {
      hits: { node: { current_version_id: string }; match: string }[]; truncated: boolean;
    };
    expect(indexed.truncated).toBe(false);
    expect(indexed.hits).toHaveLength(2);
    expect(indexed.hits.every(hit => hit.match === "content")).toBe(true);
    expect(indexed.hits.map(hit => hit.node.current_version_id).sort()).toEqual(
      rendered.filter(r => r.kind === "nested").map(r => r.version).sort(),
    );
    await writeFile(path.join(output!, "email-batch-render-proof.json"), JSON.stringify({ imported: 2, rendered }, null, 2) + "\n", { mode: 0o600 });
    await page.getByRole("button", { name: "Export completed collection", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Verified export" });
    const choose = async (label: string, option: string) => {
      await drawer.getByRole("combobox", { name: new RegExp(`^${label}`) }).click();
      await page.getByRole("option", { name: option, exact: true }).click();
    };
    await choose("Email body PDF", "Include retained body PDFs");
    await drawer.getByRole("button", { name: "Find retained PDF recipes", exact: true }).click();
    await drawer.getByRole("combobox", { name: /^Retained PDF recipe/ }).click();
    await page.getByRole("option").filter({ hasText: "A4" }).first().click();
    await choose("Email attachment outputs", "Originals + separate qualified PDFs");
    await choose("ZIP packaging", "One output / 512 MiB per volume");
    const refused = page.waitForResponse(r => new URL(r.url()).pathname === "/api/v1/exports/plans" && r.request().method() === "POST");
    await drawer.getByRole("button", { name: "Preview export", exact: true }).click();
    expect((await refused).ok()).toBe(false);
    await expect(drawer.getByRole("button", { name: "Start reviewed export", exact: true })).not.toBeVisible();
    await choose("Partial email export", "Allow declared unavailable outputs");

    for (const collapse of [false, true]) {
      if (collapse) {
        await drawer.getByRole("button", { name: "Prepare another export", exact: true }).click();
        await choose("Duplicate outputs", "Share exact duplicate outputs");
      }
      await drawer.getByRole("button", { name: "Preview export", exact: true }).click();
      await expect(drawer.getByRole("button", { name: "Start reviewed export", exact: true })).toBeEnabled({ timeout: 180_000 });
      await drawer.getByRole("button", { name: "Start reviewed export", exact: true }).click();
      await expect(drawer.getByText("Archive verified and ready", { exact: true })).toBeVisible({ timeout: 180_000 });
      const shownHash = await drawer.getByTestId("export-archive-hash").innerText();
      const downloading = page.waitForEvent("download");
      await drawer.getByRole("button", { name: "Download verified ZIP", exact: true }).click();
      const download = await downloading;
      const archive = path.join(workspace, collapse ? "collapsed.zip" : "occurrences.zip");
      await download.saveAs(archive);
      expect(await download.failure()).toBeNull();
      expect(digest(await readFile(archive))).toBe(shownHash);
      // This reader is independent of Docbank's ZIP, JSON and PDF validators.
      // It detects missing occurrences, truncated PDFs, wrong volume ordering,
      // forged checksums/counts, or collapsing outputs without explicit policy.
      const proof = JSON.parse((await exec("python3", ["-c", String.raw`
import hashlib, io, json, subprocess, sys, zipfile
sha = lambda b: hashlib.sha256(b).hexdigest()
canonical = lambda x: json.dumps(x, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()
def checked(raw):
    z = zipfile.ZipFile(raw)
    names = z.namelist()
    assert len(names) == len(set(names))
    assert all(not n.startswith('/') and '..' not in n.split('/') and '\\' not in n for n in names)
    assert z.testzip() is None
    sums = dict((line[66:], line[:64]) for line in z.read('SHA256SUMS').decode().splitlines())
    assert set(sums) == set(names) - {'SHA256SUMS'}
    for name, h in sums.items(): assert sha(z.read(name)) == h
    return z
with checked(sys.argv[1]) as z:
    manifest = json.loads(z.read('bundle.json'))
    p, docs = manifest['plan'], manifest['documents']
    fingerprint = p['fingerprint']; p['fingerprint'] = ''
    h = hashlib.sha256(canonical(p)+b'\n')
    for d in docs: h.update(canonical(d)+b'\n')
    assert h.hexdigest() == fingerprint
    assert p['source']['kind'] == 'mailbox_collection'
    assert len(docs) == p['document_rows'] == 6
    parents = [d for d in docs if 'attachment' not in d]
    assert len(parents) == p['total'] == p['source']['total'] == 2
    identities = []
    for d in docs:
        identity = d
        if 'attachment' in d:
            relation = d['attachment']
            assert all(relation['parent'][k] == d[k] for k in ('node_id','version_id','sha256','size'))
            identity = relation['child']
        identities.append(tuple(identity[k] for k in ('node_id','version_id','sha256','size')))
    assert sorted(identities) == sorted(tuple(v) for v in json.loads(sys.argv[2]))
    physical = [(d,r) for d in docs for r in d['roles'] if r['status'] == 'available']
    collapsed = [r for d in docs for r in d['roles'] if r['status'] == 'collapsed']
    unavailable = [r for d in docs for r in d['roles'] if r['status'] == 'unavailable']
    collapse = sys.argv[3] == 'true'
    assert len(physical) == p['role_entries'] == (4 if collapse else 8)
    assert len(collapsed) == (4 if collapse else 0)
    assert len(unavailable) == 2 and all(r['role']=='attachment_pdf' and not r.get('path') for r in unavailable)
    assert p['counts']['messages'] == 2 and p['counts']['attachments'] == 4
    assert p['counts']['collapsed'] == len(collapsed) and p['counts']['unavailable'] == 2
    assert p['volumes'] == len(physical)
    assert set(z.namelist()) == {'bundle.json','metadata.csv','SHA256SUMS'} | {f'volumes/{i:06d}.zip' for i in range(1,len(physical)+1)}
    outputs, pdfs, pages, role_bytes = {}, 0, 0, 0
    for i,(d,r) in enumerate(physical,1):
        with checked(io.BytesIO(z.read(f'volumes/{i:06d}.zip'))) as v:
            vm = json.loads(v.read('volume.json'))
            assert vm['index']==i and vm['first_role']==i-1 and vm['roles']==1
            assert vm['plan_fingerprint']==fingerprint and r['volume']==i
            assert set(v.namelist()) == {'volume.json','SHA256SUMS',r['path']}
            b=v.read(r['path']); assert sha(b)==r['sha256'] and len(b)==r['size']==vm['role_bytes']
            role_bytes+=len(b); outputs[r['path']]=r
            if r['role'] in ('email_pdf','attachment_pdf'):
                text=subprocess.run(['pdftotext','-','-'],input=b,stdout=subprocess.PIPE,check=True).stdout.decode()
                assert ('Complete synthetic parent body' if r['role']=='email_pdf' else 'Independently rendered nested message body') in text
                if r['role']=='email_pdf': assert 'Full original quoted body' in text
                receipt=r['recipe']; assert receipt['output']['pdf_sha256']==sha(b)
                pages+=receipt['output']['pages']; pdfs+=1
    for r in collapsed:
        previous=outputs[r['reuse_of']]
        assert r['sha256']==previous['sha256'] and r['size']==previous['size']
    assert role_bytes==p['role_bytes'] and pages==p['counts']['pages']
    assert pdfs==(2 if collapse else 4)
    print(json.dumps(dict(messages=2,attachments=4,pdfs=pdfs,pages=pages,volumes=len(physical),collapsed=len(collapsed),unavailable=2)))
`, archive, JSON.stringify(expectedMembers.map(m => [m.node_id, m.version_id, m.sha256, m.size])), String(collapse)], { timeout: 120_000, maxBuffer: 1024 * 1024 })).stdout);
      await page.screenshot({ path: path.join(output!, collapse ? "web-email-batch-collapsed.png" : "web-email-batch-occurrences.png"), fullPage: true, animations: "disabled" });
      await writeFile(path.join(output!, collapse ? "collapsed-proof.json" : "occurrences-proof.json"), JSON.stringify(proof, null, 2) + "\n", { mode: 0o600 });
      await download.delete();
    }
  } finally {
    if (started) {
      await run("daemon", "stop");
      expect((JSON.parse(await run("daemon", "status", "--json")) as { running: boolean }).running).toBe(false);
    }
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});
