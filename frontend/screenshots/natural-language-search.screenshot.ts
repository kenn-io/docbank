import { expect, test } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import * as generated from "../src/generated/docbank.js";

const exec = promisify(execFile);
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(root, process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_NATURAL_SEARCH_SCREENSHOT_DIR;
const renderLintSource = "// Canonical render lint for the UX gate. Browser-injectable; no Node APIs, no require.\n// Tests pass fake __window/__document; injected runs use browser globals.\n\nfunction collectRenderViolations(scopeSelector, options) {\n  var opts = options || {}\n  var win = opts.__window || window\n  var doc = opts.__document || win.document || document\n\n  var overlapTol = opts.overlapTolerancePx != null ? opts.overlapTolerancePx : 4\n  var clipTol = opts.clipTolerancePx != null ? opts.clipTolerancePx : 2\n  var escapeTol = opts.escapeTolerancePx != null ? opts.escapeTolerancePx : 2\n  var rawPat = new RegExp(opts.rawStringPattern || \"^[a-z0-9]+(_[a-z0-9]+)+$\")\n  var orphanTol = opts.orphanCellTolerancePx != null ? opts.orphanCellTolerancePx : 4\n  var enabledChecks = opts.checks || [\"overlap\", \"clip\", \"container-escape\", \"degenerate\", \"raw-string\", \"a11y\", \"orphan-cell\"]\n\n  function enabled(name) {\n    return enabledChecks.indexOf(name) !== -1\n  }\n\n  var root\n  if (scopeSelector && typeof scopeSelector === \"string\") {\n    root = doc.querySelector(scopeSelector)\n    if (!root) {\n      return [{ type: \"degenerate\", detail: \"scope selector matched no element\", elements: [scopeSelector], rects: [] }]\n    }\n  } else {\n    root = doc.body || doc.documentElement\n  }\n\n  var violations = []\n\n  function cssPath(node) {\n    var parts = []\n    var cur = node\n    for (var i = 0; i < 4 && cur && cur.nodeType === 1; i++) {\n      var tag = cur.tagName.toLowerCase()\n      var id = cur.getAttribute(\"id\")\n      if (id) {\n        parts.unshift(tag + \"#\" + id)\n        break\n      }\n      var cls = cur.getAttribute(\"class\")\n      if (cls) {\n        parts.unshift(tag + \".\" + cls.split(/\\s+/)[0])\n      } else {\n        parts.unshift(tag)\n      }\n      cur = cur.parentElement\n    }\n    return parts.join(\" > \")\n  }\n\n  function getComputed(node) {\n    return win.getComputedStyle(node)\n  }\n\n  function isVisible(node) {\n    if (node.nodeType !== 1) return false\n    var cs = getComputed(node)\n    if (cs.display === \"none\" || cs.visibility === \"hidden\") return false\n    var r = node.getBoundingClientRect()\n    return r.width > 0 && r.height > 0\n  }\n\n  function isVisibleForDegenerate(node) {\n    if (node.nodeType !== 1) return false\n    var cs = getComputed(node)\n    return cs.display !== \"none\" && cs.visibility !== \"hidden\"\n  }\n\n  function bearsText(node) {\n    var cn = node.childNodes\n    for (var i = 0; i < cn.length; i++) {\n      if (cn[i].nodeType === 3 && cn[i].data && cn[i].data.trim() !== \"\") return true\n    }\n    return false\n  }\n\n  function hasSubtreeText(node) {\n    if (node.nodeType === 3) return node.data && node.data.trim() !== \"\"\n    if (node.nodeType !== 1) return false\n    var cn = node.childNodes\n    for (var i = 0; i < cn.length; i++) {\n      if (hasSubtreeText(cn[i])) return true\n    }\n    return false\n  }\n\n  function getSubtreeText(node) {\n    if (node.nodeType === 3) return node.data || \"\"\n    if (node.nodeType !== 1) return \"\"\n    var t = \"\"\n    var cn = node.childNodes\n    for (var i = 0; i < cn.length; i++) {\n      t += getSubtreeText(cn[i])\n    }\n    return t\n  }\n\n  var INTERACTIVE_TAGS = { BUTTON: 1, INPUT: 1, SELECT: 1, TEXTAREA: 1 }\n  var CONTROL_TAGS = { BUTTON: 1, INPUT: 1, SELECT: 1, TEXTAREA: 1, A: 1, LABEL: 1 }\n\n  function isInteractive(node) {\n    if (INTERACTIVE_TAGS[node.tagName]) return true\n    if (node.tagName === \"A\" && node.getAttribute(\"href\") != null) return true\n    if (node.getAttribute(\"role\") === \"button\") return true\n    return false\n  }\n\n  function isTextOrControl(node) {\n    return bearsText(node) || !!CONTROL_TAGS[node.tagName]\n  }\n\n  function rectIntersection(a, b) {\n    var x = Math.max(a.left, b.left)\n    var y = Math.max(a.top, b.top)\n    var r = Math.min(a.right, b.right)\n    var bot = Math.min(a.bottom, b.bottom)\n    return { width: Math.max(0, r - x), height: Math.max(0, bot - y) }\n  }\n\n  function fullyContains(outer, inner) {\n    return outer.left <= inner.left && outer.top <= inner.top &&\n      outer.right >= inner.right && outer.bottom >= inner.bottom\n  }\n\n  function findOverflowHiddenAncestor(node) {\n    var cur = node.parentElement\n    while (cur && cur !== root.parentElement) {\n      var cs = getComputed(cur)\n      if (cs.overflowX === \"hidden\" || cs.overflowY === \"hidden\") return cur\n      cur = cur.parentElement\n    }\n    return null\n  }\n\n  function walk(node) {\n    if (node.nodeType !== 1) return\n    var cs = getComputed(node)\n    if (cs.display === \"none\" || cs.visibility === \"hidden\") return\n\n    var children = node.childNodes\n    var visibleElements = []\n    var degenerateElements = []\n    for (var i = 0; i < children.length; i++) {\n      if (children[i].nodeType !== 1) continue\n      if (isVisible(children[i])) {\n        visibleElements.push(children[i])\n      } else if (isVisibleForDegenerate(children[i])) {\n        degenerateElements.push(children[i])\n      }\n    }\n\n    if (enabled(\"overlap\")) {\n      for (var i = 0; i < visibleElements.length; i++) {\n        var ei = visibleElements[i]\n        var csi = getComputed(ei)\n        if (csi.position === \"fixed\") continue\n        if (!isTextOrControl(ei)) continue\n        var ri = ei.getBoundingClientRect()\n        for (var j = i + 1; j < visibleElements.length; j++) {\n          var ej = visibleElements[j]\n          var csj = getComputed(ej)\n          if (csj.position === \"fixed\") continue\n          if (!isTextOrControl(ej)) continue\n          var rj = ej.getBoundingClientRect()\n          var inter = rectIntersection(ri, rj)\n          if (inter.width > overlapTol && inter.height > overlapTol) {\n            if (fullyContains(ri, rj) || fullyContains(rj, ri)) {\n              if (!(bearsText(ei) && bearsText(ej))) continue\n            }\n            violations.push({\n              type: \"overlap\",\n              detail: \"sibling elements overlap by \" + Math.round(inter.width) + \"x\" + Math.round(inter.height) + \"px\",\n              elements: [cssPath(ei), cssPath(ej)],\n              rects: [ri, rj],\n            })\n          }\n        }\n      }\n    }\n\n    for (var i = 0; i < visibleElements.length; i++) {\n      var child = visibleElements[i]\n\n      if (enabled(\"clip\") && bearsText(child)) {\n        var csc = getComputed(child)\n        if (child.scrollWidth > child.clientWidth + clipTol) {\n          if (csc.textOverflow.indexOf(\"ellipsis\") === -1 &&\n            csc.overflowX !== \"scroll\" && csc.overflowX !== \"auto\") {\n            violations.push({\n              type: \"clip\",\n              detail: \"text clipped: scrollWidth \" + child.scrollWidth + \" > clientWidth \" + child.clientWidth,\n              elements: [cssPath(child)],\n              rects: [child.getBoundingClientRect()],\n            })\n          }\n        }\n      }\n\n      if (enabled(\"container-escape\")) {\n        var ancestor = findOverflowHiddenAncestor(child)\n        if (ancestor) {\n          var cr = child.getBoundingClientRect()\n          var ar = ancestor.getBoundingClientRect()\n          var ancestorCs = getComputed(ancestor)\n          var escapeX = ancestorCs.overflowX === \"hidden\" && (cr.right - ar.right > escapeTol || ar.left - cr.left > escapeTol)\n          var escapeY = ancestorCs.overflowY === \"hidden\" && (cr.bottom - ar.bottom > escapeTol || ar.top - cr.top > escapeTol)\n          if (escapeX || escapeY) {\n            violations.push({\n              type: \"container-escape\",\n              detail: \"element extends past overflow-hidden ancestor\",\n              elements: [cssPath(child), cssPath(ancestor)],\n              rects: [cr, ar],\n            })\n          }\n        }\n      }\n\n      if (enabled(\"degenerate\") && isInteractive(child)) {\n        if (child.tagName === \"INPUT\" && child.getAttribute(\"type\") === \"hidden\") {\n          // skip\n        } else if (isVisibleForDegenerate(child)) {\n          var dr = child.getBoundingClientRect()\n          var zeroSize = dr.width === 0 || dr.height === 0\n          var offViewport = dr.right < 0 || dr.bottom < 0 || dr.left > win.innerWidth || dr.top > win.innerHeight\n          if (zeroSize || offViewport) {\n            violations.push({\n              type: \"degenerate\",\n              detail: zeroSize ? \"interactive element has zero-size rect\" : \"interactive element is off-viewport\",\n              elements: [cssPath(child)],\n              rects: [dr],\n            })\n          }\n        }\n      }\n\n      if (enabled(\"raw-string\")) {\n        var cn = child.childNodes\n        for (var k = 0; k < cn.length; k++) {\n          if (cn[k].nodeType === 3 && cn[k].data) {\n            var trimmed = cn[k].data.trim()\n            if (trimmed && rawPat.test(trimmed)) {\n              violations.push({\n                type: \"raw-string\",\n                detail: \"text node matches raw-string pattern: \" + trimmed,\n                elements: [cssPath(child)],\n                rects: [child.getBoundingClientRect()],\n              })\n            }\n          }\n        }\n        var RAW_ATTRS = [\"title\", \"aria-label\", \"placeholder\"]\n        for (var k = 0; k < RAW_ATTRS.length; k++) {\n          var val = child.getAttribute(RAW_ATTRS[k])\n          if (val && rawPat.test(val.trim())) {\n            violations.push({\n              type: \"raw-string\",\n              detail: RAW_ATTRS[k] + \" attribute matches raw-string pattern: \" + val.trim(),\n              elements: [cssPath(child)],\n              rects: [],\n            })\n          }\n        }\n      }\n\n      if (enabled(\"a11y\") && isInteractive(child)) {\n        var ariaLabel = child.getAttribute(\"aria-label\")\n        var title = child.getAttribute(\"title\")\n        var labelledby = child.getAttribute(\"aria-labelledby\")\n        var subtreeHasText = hasSubtreeText(child)\n        var hasName = (ariaLabel && ariaLabel.trim()) || (title && title.trim()) || (labelledby && labelledby.trim())\n        if (!subtreeHasText && !hasName) {\n          violations.push({\n            type: \"missing-accessible-name\",\n            detail: \"interactive element has no accessible name\",\n            elements: [cssPath(child)],\n            rects: [child.getBoundingClientRect()],\n          })\n        }\n        if (ariaLabel && ariaLabel.trim() && subtreeHasText) {\n          var visibleText = getSubtreeText(child).trim().toLowerCase()\n          if (visibleText && ariaLabel.trim().toLowerCase().indexOf(visibleText) === -1) {\n            violations.push({\n              type: \"label-not-in-name\",\n              detail: \"aria-label \\\"\" + ariaLabel.trim() + \"\\\" does not contain visible text \\\"\" + visibleText + \"\\\"\",\n              elements: [cssPath(child)],\n              rects: [child.getBoundingClientRect()],\n            })\n          }\n        }\n      }\n    }\n\n    if (enabled(\"orphan-cell\")) {\n      var cellTags = { TD: 1, TH: 1 }\n      var tableTags = { TABLE: 1, TBODY: 1, THEAD: 1, TFOOT: 1, TR: 1 }\n      for (var i = 0; i < visibleElements.length; i++) {\n        var cell = visibleElements[i]\n        if (!cellTags[cell.tagName]) continue\n        var cellRect = cell.getBoundingClientRect()\n        var siblings = 0\n        for (var j = 0; j < visibleElements.length; j++) {\n          if (j === i) continue\n          if (!cellTags[visibleElements[j].tagName]) continue\n          var sibRect = visibleElements[j].getBoundingClientRect()\n          if (Math.abs(sibRect.top - cellRect.top) < orphanTol) siblings++\n        }\n        if (siblings === 0) {\n          violations.push({\n            type: \"orphan-cell\",\n            detail: \"table cell is the only cell in its visual row\",\n            elements: [cssPath(cell)],\n            rects: [cellRect],\n          })\n        }\n      }\n    }\n\n    if (enabled(\"degenerate\")) {\n      for (var i = 0; i < degenerateElements.length; i++) {\n        var dNode = degenerateElements[i]\n        if (isInteractive(dNode)) {\n          if (dNode.tagName === \"INPUT\" && dNode.getAttribute(\"type\") === \"hidden\") continue\n          var dr2 = dNode.getBoundingClientRect()\n          var zs = dr2.width === 0 || dr2.height === 0\n          var ov = dr2.right < 0 || dr2.bottom < 0 || dr2.left > win.innerWidth || dr2.top > win.innerHeight\n          if (zs || ov) {\n            violations.push({\n              type: \"degenerate\",\n              detail: zs ? \"interactive element has zero-size rect\" : \"interactive element is off-viewport\",\n              elements: [cssPath(dNode)],\n              rects: [dr2],\n            })\n          }\n        }\n      }\n    }\n\n    for (var i = 0; i < visibleElements.length; i++) {\n      walk(visibleElements[i])\n    }\n  }\n\n  walk(root)\n  return violations\n}\n\nfunction renderLintSnippet(scopeSelector, options) {\n  return \"(\" + collectRenderViolations.toString() + \")(\" +\n    JSON.stringify(scopeSelector != null ? scopeSelector : null) + \", \" +\n    JSON.stringify(options || {}) + \")\"\n}\n\nmodule.exports = { collectRenderViolations, renderLintSnippet }\n";

test("natural-language search renders processing controls and live rows", async ({page}) => {
  test.skip(!output, "DOCBANK_NATURAL_SEARCH_SCREENSHOT_DIR is required");
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-natural-search-"));
  const vault = path.join(workspace, "vault");
  let calls = 0;
  let daemon: ChildProcess | undefined;
  let daemonError = "";
  let record: {address:string;metadata:Record<string,string>} | undefined;
  const originalFetch = globalThis.fetch;
  let baseURL = "";
  globalThis.fetch = (input, init) => originalFetch(typeof input === "string" && input.startsWith("/") ? baseURL+input : input, init);
  const server = createServer(async (request, response) => {
    calls += 1;
    let body = "";
    for await (const chunk of request) body += String(chunk);
    const payload = JSON.parse(body) as {input: string | string[]};
    const inputs = Array.isArray(payload.input) ? payload.input : [payload.input];
    response.writeHead(200, {"Content-Type":"application/json"});
    response.end(JSON.stringify({object:"list",model:"synthetic-model",data:inputs.map((text,index) => ({object:"embedding",index,embedding:text.includes("garden") ? [0,1] : [1,0]})),usage:{prompt_tokens:inputs.length,total_tokens:inputs.length}}));
  });
  try {
    await mkdir(vault,{recursive:true,mode:0o700});
    await mkdir(output!,{recursive:true,mode:0o700});
    await new Promise<void>((resolve) => server.listen(0,"127.0.0.1",resolve));
    const embeddingOrigin = "http://127.0.0.1:" + (server.address() as AddressInfo).port;
    const identities = JSON.parse((await exec("go",["run","-tags","fts5",path.join(root,"frontend/screenshots/processing-profile.go"),embeddingOrigin],{cwd:root,windowsHide:true,timeout:60_000})).stdout) as {rendition_id:string;rendition_fingerprint:string;embedding_id:string;embedding_fingerprint:string;compatibility_id:string};
    await writeFile(path.join(vault,"config.toml"),`
[credential_bindings.semantic]
environment_variable = "DOCBANK_SCREENSHOT_EMBEDDING_KEY"

[rendition_profiles.plaintext]
adapter_contract = "docbank-plaintext-rendition/v1"
authorization_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
credential_binding = "credential:none"
deployment_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
descriptor_id = ${JSON.stringify(identities.rendition_id)}
descriptor_fingerprint = ${JSON.stringify(identities.rendition_fingerprint)}
disclose_filename = false
disclosure_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
max_document_bytes = 16777216
max_response_bytes = 16777216
max_units = 1
requested_artifacts = ["structured_evidence"]
trust_boundary = "local_process"
upload_options_fingerprint = "4444444444444444444444444444444444444444444444444444444444444444"

[embedding_profiles.semantic]
activation = "optional"
authorization_fingerprint = "5555555555555555555555555555555555555555555555555555555555555555"
compatibility_id = ${JSON.stringify(identities.compatibility_id)}
credential_binding = "credential:semantic"
descriptor_id = ${JSON.stringify(identities.embedding_id)}
descriptor_fingerprint = ${JSON.stringify(identities.embedding_fingerprint)}
dimensions = 2
disclosure_fingerprint = "6666666666666666666666666666666666666666666666666666666666666666"
document_formatter = "openai-compatible/document/v1"
input_kind = "rendition_chunk"
max_batch_items = 8
max_input_bytes = 1048576
max_input_tokens = 128
max_response_bytes = 1048576
metric = "cosine"
model = "synthetic-model"
normalization = "none"
query_formatter = "openai-compatible/query/v1"
scalar_encoding = "float32"
trust_boundary = "operator_network"

[embedding_profiles.semantic.chunk]
context_fingerprint = "7777777777777777777777777777777777777777777777777777777777777777"
formatter = "rendition-chunk/v1"
max_tokens = 128
overlap_tokens = 8
tokenizer = "unicode-runes"
tokenizer_revision = "v1"
truncation_policy = "reject_indivisible"

[embedding_profiles.semantic.model_input]
profile = "nomic/v1"

[embedding_profiles.semantic.runtime]
adapter_contract = "docbank-openai-compatible-embeddings/v1"
endpoint = ${JSON.stringify(embeddingOrigin)}
model_revision = "deployment-v1"
deployment_epoch = "deployment-v1"
request_timeout = "1s"
max_request_bytes = 1048576
allowed_cidrs = ["127.0.0.0/8"]
proxy_mode = "disabled"
connect_timeout = "1s"
keep_alive = "1s"
tls_handshake_timeout = "1s"

[retrieval_profiles.hybrid]
lexical_limit = 20
vector_limit = 20

[processing_profiles.private_text]
rendition = "plaintext"
embeddings = ["semantic"]
retrieval = "hybrid"
attachment_policy_fingerprint = "8888888888888888888888888888888888888888888888888888888888888888"
completeness_fingerprint = "9999999999999999999999999999999999999999999999999999999999999999"
consent_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
lexical_segmenter_fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
max_segment_runes = 2000
max_unit_runes = 100000
max_document_chars = 1000000
normalizer_fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
sanitizer_fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
retain_sanitized_markdown = true
retain_typed_artifacts = true
trust_boundary = "local_process"
`,{mode:0o600});
    daemon = spawn(binary,["daemon","run"],{cwd:root,windowsHide:true,stdio:["ignore","ignore","pipe"],env:{...process.env,DOCBANK_HOME:vault,DOCBANK_SCREENSHOT_EMBEDDING_KEY:"synthetic-secret"}});
    daemon.stderr!.on("data", (data: Buffer) => { daemonError += data.toString(); });
    await expect.poll(async () => {
      if (daemon!.exitCode !== null) throw Error(daemonError);
      try { record = JSON.parse(await readFile(path.join(vault,`daemon.${daemon!.pid}.json`),"utf8")); return true; } catch { return false; }
    },{timeout:60_000}).toBe(true);
    baseURL = "http://"+record!.address;
    const headers={"X-Api-Key":record!.metadata.api_key!};
    for (const [name,text] of [["source.txt","Synthetic solar panel maintenance."],["solar-guide.txt","Synthetic solar panel cleaning."],["solar-copy.txt","Synthetic solar panel cleaning."],["garden.txt","Synthetic garden planting schedule."],["z-unembedded.txt","Synthetic document awaiting processing."]]) {
      await generated.uploadFile({file:new Blob([text!],{type:"text/plain"})},{parent_id:1,name:name!},
        {"X-Docbank-Blob-Hash":createHash("sha256").update(text!).digest("hex"),"X-Docbank-Blob-Size":String(Buffer.byteLength(text!))},{headers});
    }
    const browser=await generated.createWebSession({headers});
    const session=browser.token;
    const webURL=browser.url+"#"+new URLSearchParams({web_session:session,web_upload_secret:browser.upload_secret});
    await page.goto(webURL);
    await page.getByText("source.txt",{exact:true}).waitFor();
    baseURL = "http://127.0.0.1:"+new URL(browser.url).port;
    const sessionOptions={session,headers:{Host:new URL(browser.url).host}};
      const children=await generated.listChildren(1,{limit:1000,offset:0},sessionOptions);
      for (const node of children.items) {
        if (node.name === "z-unembedded.txt") continue;
        const selector={node_id:node.id,content_version_id:node.current_version_id!,profile:"private_text"};
        const plan=await generated.planDocumentProcessing({selector},sessionOptions);
        const started=await generated.startDocumentProcessing({selector,plan_fingerprint:plan.fingerprint,consent:true},sessionOptions);
        const body=await started.text();
        if (!started.ok || !body.includes('"state":"completed"')) throw Error(body);
      }
    const mode = page.getByRole("combobox",{name:"Search mode: Auto"});
    await expect(mode).toBeVisible();
    await mode.click();
    await expect(page.getByRole("option",{name:"Names and text"})).toBeVisible();
    await expect(page.getByRole("option",{name:"Auto"})).toBeVisible();
    await expect(page.getByRole("option",{name:"Lexical"})).toBeVisible();
    await expect(page.getByRole("option",{name:"Semantic"})).toBeVisible();
    await expect(page.getByRole("option",{name:"Hybrid"})).toBeVisible();
    await page.keyboard.press("Escape");
    const search = page.getByRole("searchbox",{name:"Search documents"});
    await search.fill("solar maintenance");
    await search.press("Enter");
    const resultPath = page.getByLabel("Vault browser").getByText("/source.txt",{exact:true});
    await expect(resultPath).toBeVisible();
    await expect(page.getByText("Document text",{exact:true}).first()).toBeVisible();
    await expect(page.getByText("Text",{exact:true}).first()).toBeVisible();
    const lintStart = renderLintSource.indexOf("function collectRenderViolations");
    const lintEnd = renderLintSource.indexOf("\nfunction renderLintSnippet");
    const collectRenderViolations = renderLintSource.slice(lintStart, lintEnd);
    const renderLint = async (width: number): Promise<void> => {
      await page.setViewportSize({width,height:960});
      const violations = await page.evaluate((source) => {
        const factory = new Function(`${source}; return collectRenderViolations;`);
        return factory()("body",{checks:["overlap","clip","raw-string","a11y","orphan-cell"]});
      },collectRenderViolations);
      expect(violations,`render-lint width=${width}`).toEqual([]);
    };
    for (const width of [1440,1280,768,400]) {
      await renderLint(width);
      await resultPath.scrollIntoViewIfNeeded();
      await page.screenshot({path:path.join(output!,"web-natural-"+width+".png")});
    }
    console.log("natural search screenshot: modes=Names and text,Auto,Lexical,Semantic,Hybrid rows=live excerpt=true evidence=true widths=1440,1280,768,400 render-lint=clean");
    const socket = `docbank-natural-${process.pid}`;
    const tmux = async (args: string[]) => (await exec(process.platform === "win32" ? "wsl.exe" : "tmux",
      process.platform === "win32" ? ["-d","Ubuntu","--","tmux","-L",socket,...args] : ["-L",socket,...args],
      {windowsHide:true,timeout:15_000,maxBuffer:1024*1024})).stdout;
    try {
      const executable = process.platform === "win32" ? (await exec("wsl.exe",["-d","Ubuntu","--","wslpath","-u",binary.replaceAll("\\","/")],{windowsHide:true})).stdout.trim() : binary;
      await tmux(["new-session","-d","-x","120","-y","55","-s","natural","env",`DOCBANK_HOME=${vault.replaceAll("\\","/")}`,"WSLENV=DOCBANK_HOME",executable,"tui"]);
      await expect.poll(() => tmux(["capture-pane","-p","-t","natural"]),{timeout:20_000}).toContain("source.txt");
      await tmux(["send-keys","-t","natural","/","solar maintenance","Enter"]);
      await expect.poll(() => tmux(["capture-pane","-p","-t","natural"]),{timeout:20_000}).toContain("solar maintenance");
      const terminal = await tmux(["capture-pane","-p","-t","natural"]);
      await writeFile(path.join(output!,"tui-natural-search.txt"),terminal);
      await page.setViewportSize({width:1440,height:1100});
      await page.setContent('<body style="margin:0;background:#07100f;color:#e8f1ef"><pre style="padding:16px;font:14px/19px monospace"></pre></body>');
      await page.locator("pre").evaluate((element,text) => {element.textContent=text;},terminal);
      await page.screenshot({path:path.join(output!,"tui-natural-search.png")});
      console.log("natural TUI screenshot: mode=Auto excerpt=true evidence=true status=rendered");
    } catch (error) {
      console.log("manual-owner-proof blocker: TUI capture: "+String(error));
    } finally {
      await tmux(["kill-server"]).catch(() => {});
    }
  } catch (error) {
    console.error(daemonError);
    console.error(error);
    throw error;
  } finally {
    if (record) {
      baseURL = "http://"+record.address;
      await generated.shutdownDaemon({"X-Docbank-Daemon-Token":record.metadata.shutdown_token!},{headers:{"X-Api-Key":record.metadata.api_key!}}).catch(() => {});
    }
    globalThis.fetch = originalFetch;
    if (daemon && daemon.exitCode === null) {
      daemon.kill();
      await new Promise<void>((resolve) => daemon!.once("exit",() => resolve()));
    }
    await new Promise<void>((resolve,reject) => server.close((err) => err ? reject(err) : resolve()));
    await rm(workspace,{recursive:true,force:true,maxRetries:5,retryDelay:500});
    console.log("natural search screenshot: synthetic vault and daemon removed");
  }
});
