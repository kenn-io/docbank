import { readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";

// Remove these corrections when EmbedPDF scopes shortcuts to its viewer and
// destroys engines that finish initializing after unmount. Browser proofs cover both.
const directory = new URL("../node_modules/@embedpdf/snippet/", import.meta.url);
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
const payloadName = "docbank-page-hooks.mjs";
const payloadSource = new URL("./embedpdf-page-hooks.mjs", import.meta.url);
const moduleImport = 'import { createPageHooks } from "./docbank-page-hooks.mjs";\n';
const setup = `\nconst {RenderLayer:DocbankRenderLayer,TileImg:DocbankTileImg,PageLayer:DocbankPageLayer}=createPageHooks({h:j,Fragment:B,useLayoutEffect:Pe,useRef:De,useState:Se,useRenderCapability:()=>Xo(ws.id),useDocumentState:Yo,useTilingCapability:yc,cancelError:()=>({code:pn.Cancelled,message:"page image disposed"})});\n`;

const sections = [
  [
    "function Cs(",
    "const Is=",
    "2f33a17d841ab1f0284aad11288fa355b5836a5fa87a23a29b622f1ef6454255",
    "function Cs(props){return DocbankRenderLayer(props)}",
  ],
  [
    "function bc(",
    "function wc(",
    "5560e4a44ec2128ccf494e0801f5903dc4bf91d0f887e3c43f0744db7dbd4476",
    "function bc(props){return DocbankTileImg(props)}",
  ],
];
replacements.push([
  "j(zm,{documentId:t,pageIndex:n},j(Cs,",
  "j(zm,{documentId:t,pageIndex:n},j(DocbankPageLayer,{documentId:t,pageIndex:n}),j(Cs,",
]);

async function patchedCode(root = directory) {
  const { version } = JSON.parse(await readFile(new URL("package.json", root), "utf8"));
  if (version !== "2.15.1") throw new Error("Review the EmbedPDF extensions before upgrading.");
  const bundle = new URL("dist/embedpdf-cl9qiF45.js", root);
  let code = await readFile(bundle, "utf8");
  const original = code;
  for (const [before, after] of replacements) {
    if (code.includes(after)) continue;
    if (code.split(before).length !== 2)
      throw new Error("Unexpected EmbedPDF bundle; review the extensions.");
    code = code.replace(before, after);
  }
  for (const [start, end, hash, replacement] of sections) {
    if (code.includes(replacement)) continue;
    const from = code.indexOf(start),
      to = code.indexOf(end, from);
    if (
      from < 0 ||
      to < from ||
      createHash("sha256").update(code.slice(from, to)).digest("hex") !== hash
    )
      throw new Error("Unexpected EmbedPDF render component; review the extensions.");
    code = code.slice(0, from) + replacement + code.slice(to);
  }
  if (!code.startsWith(moduleImport)) code = moduleImport + code;
  if (!code.endsWith(setup)) code += setup;
  return { bundle, code, original };
}

export async function verifyEmbedPDFPatch(root = directory) {
  const { code, original } = await patchedCode(root);
  const expected = await readFile(payloadSource, "utf8");
  let payload;
  try {
    payload = await readFile(new URL(`dist/${payloadName}`, root), "utf8");
  } catch {
    /* Missing patch is reported below. */
  }
  if (code !== original || payload !== expected)
    throw new Error(
      "EmbedPDF extensions are missing. Run npm --prefix frontend run postinstall, then rebuild.",
    );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { bundle, code } = await patchedCode();
  await writeFile(bundle, code);
  await writeFile(new URL(`dist/${payloadName}`, directory), await readFile(payloadSource));
}
