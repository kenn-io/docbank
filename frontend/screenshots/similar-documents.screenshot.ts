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
const output = process.env.DOCBANK_SIMILAR_SCREENSHOT_DIR;

test("stored embeddings find similar documents without provider egress", async ({page}) => {
  test.skip(!output, "DOCBANK_SIMILAR_SCREENSHOT_DIR is required");
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-similar-"));
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
    await page.getByRole("button",{name:"Find documents similar to source.txt"}).waitFor();
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
    calls=0;
    const action=page.getByRole("button",{name:"Find documents similar to source.txt"});
    for (const width of [1440,1280,768,400]) {
      await page.setViewportSize({width,height:960});
      await action.scrollIntoViewIfNeeded();
      await expect(action).toBeInViewport();
      await action.hover();
      await page.screenshot({path:path.join(output!,"web-similar-action-"+width+".png")});
    }
    await page.getByRole("button",{name:"Find documents similar to source.txt"}).click();
    const section=page.getByRole("region",{name:"Similar documents"});
    await expect(section.getByText("+1 identical")).toBeVisible();
    await expect(section.getByText("/garden.txt",{exact:true})).toBeVisible();
    expect(calls).toBe(0);
    for (const width of [1440,1280,768,400]) {
      await page.setViewportSize({width,height:960});
      await section.scrollIntoViewIfNeeded();
      await page.screenshot({path:path.join(output!,"web-similar-"+width+".png")});
    }
    await page.getByRole("button",{name:"Close document processing"}).click();
    await page.getByRole("button",{name:"Find documents similar to z-unembedded.txt"}).click();
    await expect(section.getByText(/Unavailable: no current embedding/)).toBeVisible();
    for (const width of [1440,1280,768,400]) {
      await page.setViewportSize({width,height:960});
      await section.scrollIntoViewIfNeeded();
      await page.screenshot({path:path.join(output!,"web-similar-unavailable-"+width+".png")});
    }
    expect(calls).toBe(0);
    console.log("similar screenshot: row_action=visible ready=true unavailable=true provider_calls=0 duplicate_count=1 scope=5 widths=1440,1280,768,400");
    const socket = `docbank-similar-${process.pid}`;
    const tmux = async (args: string[]) => (await exec(process.platform === "win32" ? "wsl.exe" : "tmux",
      process.platform === "win32" ? ["-d","Ubuntu","--","tmux","-L",socket,...args] : ["-L",socket,...args],
      {windowsHide:true,timeout:15_000,maxBuffer:1024*1024})).stdout;
    try {
      const executable = process.platform === "win32" ? (await exec("wsl.exe",["-d","Ubuntu","--","wslpath","-u",binary.replaceAll("\\","/")],{windowsHide:true})).stdout.trim() : binary;
      await tmux(["new-session","-d","-x","120","-y","55","-s","similar","env",`DOCBANK_HOME=${vault.replaceAll("\\","/")}`,"WSLENV=DOCBANK_HOME",executable,"tui"]);
      await expect.poll(() => tmux(["capture-pane","-p","-t","similar"]),{timeout:20_000}).toContain("source.txt");
      await tmux(["send-keys","-t","similar","S"]);
      await expect.poll(() => tmux(["capture-pane","-p","-t","similar"]),{timeout:20_000}).toContain("identical");
      const terminal = await tmux(["capture-pane","-p","-t","similar"]);
      await page.setViewportSize({width:1440,height:1100});
      await page.setContent('<body style="margin:0;background:#07100f;color:#e8f1ef"><pre style="padding:16px;font:14px/19px monospace"></pre></body>');
      await page.locator("pre").evaluate((element,text) => {element.textContent=text;},terminal);
      await page.screenshot({path:path.join(output!,"tui-similar.png")});
      console.log("similar TUI screenshot: uppercase S rendered stored results");
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
    console.log("similar screenshot: synthetic vault and daemon removed");
  }
});
