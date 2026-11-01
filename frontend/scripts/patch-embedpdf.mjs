import { readFile, writeFile } from "node:fs/promises";

// Remove these corrections when EmbedPDF scopes shortcuts to its viewer and
// destroys engines that finish initializing after unmount. Browser proofs cover both.
const directory = new URL("../node_modules/@embedpdf/snippet/", import.meta.url);
const { version } = JSON.parse(await readFile(new URL("package.json", directory), "utf8"));
if (version !== "2.15.1") throw new Error("Review the EmbedPDF lifecycle fixes before upgrading.");
const bundle = new URL("dist/embedpdf-cl9qiF45.js", directory);
let code = await readFile(bundle, "utf8");
const replacements = [
  [
    "const{provides:e}=Ua();return Me(()=>{if(!e)return;const t=function(e)",
    "const{provides:e}=Ua(),shortcutRoot=De(null);return Me(()=>{if(!e)return;const t=function(e)",
  ],
  [
    'return document.addEventListener("keydown",t),()=>document.removeEventListener("keydown",t)},[e]),null}).build(),Ha=',
    'const root=shortcutRoot.current.getRootNode().host;return root.addEventListener("keydown",t),()=>root.removeEventListener("keydown",t)},[e]),j("span",{ref:shortcutRoot,hidden:!0})}).build(),Ha=',
  ],
  [
    'const{createPdfiumEngine:e}=n?await import("./worker-engine-BCCrxWYm.js")',
    'const{createPdfiumEngine:createEngine}=n?await import("./worker-engine-BCCrxWYm.js")',
  ],
  [
    "a=await e(t,{logger:o,encoderPoolSize:i,fontFallback:r});h.current=a,s(a),c(!1)",
    "a=await createEngine(t,{logger:o,encoderPoolSize:i,fontFallback:r});if(e){a.destroy();return}h.current=a,s(a),c(!1)",
  ],
];
for (const [before, after] of replacements) {
  if (code.includes(after)) continue;
  if (code.split(before).length !== 2)
    throw new Error("Unexpected EmbedPDF bundle; review the fixes.");
  code = code.replace(before, after);
}
await writeFile(bundle, code);
