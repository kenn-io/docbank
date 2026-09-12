import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_QUERY_BAR_SCREENSHOT_DIR;
test.skip(!screenshots, "DOCBANK_QUERY_BAR_SCREENSHOT_DIR is required for PR-only query-bar captures");
test("complete query validation uses the real compiler without executing", async ({page}) => {
  const workspace = await mkdtemp(path.join(tmpdir(),"docbank-query-bar-"));
  const run = async (...args:string[]) => (await exec(path.join(repository,"docbank"),args,{
    cwd:repository,env:{...process.env,DOCBANK_HOME:path.join(workspace,"vault")},timeout:60_000,
  })).stdout.trim();
  try {
    await writeFile(path.join(workspace,"manual.txt"),"Synthetic field manual and archive notes.\n",{mode:0o600});
    await run("add",path.join(workspace,"manual.txt"),"--dest","/");
    await page.goto(await run("web","--no-browser"));
    await page.getByRole("button",{name:"Import collections",exact:true}).click();
    const collections=page.getByRole("dialog",{name:"Import collections"});
    await collections.getByRole("button",{name:/Browse collection/}).first().click();
    await collections.getByRole("button",{name:"New query for this collection"}).click();
    const editor=page.getByRole("region",{name:"Query editor"});
    await expect(editor.getByText(/Query validated/)).toBeVisible();
    const initial=JSON.parse(new URLSearchParams(new URL(page.url()).hash.slice(1)).get("query")!);
    expect(initial.filters.collection_ids).toHaveLength(1);
    const searches:string[]=[];
    page.on("request",(request)=>{if(new URL(request.url()).pathname==="/api/v1/search") searches.push(request.url());});
    await editor.getByRole("combobox",{name:"Query syntax: Simple"}).click();
    await page.getByRole("option",{name:"Advanced",exact:true}).click();
    await editor.getByLabel("Query expression",{exact:true}).fill("name:manual OR archive");
    await expect(editor.getByText(/Query validated/)).toBeVisible();
    await mkdir(screenshots!,{recursive:true,mode:0o700});
    await page.screenshot({path:path.join(screenshots!,"web-query-bar.png"),fullPage:true});
    await editor.getByLabel("Query expression",{exact:true}).fill("NOT tag:missing-tag");
    await expect(editor.getByRole("alert")).toBeVisible();
    await expect(editor.getByRole("button",{name:"Run query"})).toBeDisabled();
    await editor.getByRole("button",{name:"Focus query error"}).click();
    await expect(editor.getByLabel("Query expression",{exact:true})).toBeFocused();
    await page.screenshot({path:path.join(screenshots!,"web-query-error.png"),fullPage:true});
    await editor.getByRole("button",{name:"Save query draft"}).click();
    const saved=page.getByRole("dialog",{name:"Saved queries and highlights"});
    await saved.getByLabel("Definition name",{exact:true}).fill("Synthetic review");
    await saved.getByRole("button",{name:"Save as new",exact:true}).click();
    await expect(saved.getByText("Saved Synthetic review.",{exact:true})).toBeVisible();
    await saved.getByRole("button",{name:"Open query Synthetic review"}).click();
    await expect(editor.getByLabel("Query expression",{exact:true})).toHaveValue("NOT tag:missing-tag");
    const retained=JSON.parse(new URLSearchParams(new URL(page.url()).hash.slice(1)).get("query")!);
    expect(retained.filters).toEqual(initial.filters);
    expect(searches).toEqual([]);
  } finally {
    await run("daemon","stop");
    const status=JSON.parse(await run("daemon","status","--json"));
    if(status.running!==false) throw new Error("Synthetic query daemon remains running; workspace retained.");
    await rm(workspace,{recursive:true,force:true});
  }
});
