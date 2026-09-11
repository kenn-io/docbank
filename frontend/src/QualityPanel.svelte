<script lang="ts">
  import { Button, Checkbox, Chip, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { collectionQuality, qualitySuggestion, type CollectionQuality } from "./collectionQuality.js";
  import type { Query } from "./query.js";
  interface Props {session:string;collectionID:string;profile:string;onprofile:(name:string)=>void;onnewquery?:(query:Query)=>void;onauthfailure:(cause:unknown)=>void}
  let {session,collectionID,profile,onprofile,onnewquery,onauthfailure}:Props=$props();
  let result=$state<CollectionQuality>();
  let error=$state("");let loading=$state(false);let revision=$state(0);
  let selected=$state<Record<string,string[]>>({});
  let generation=0;
  const suggestible=new Set(["extension","media_type","media_family","text_coverage","duplicates"]);
  $effect(()=>{
    const activeSession=session,id=collectionID,name=profile;void revision;
    const request=++generation,abort=new AbortController();
    result=undefined;error="";selected={};loading=true;
    void collectionQuality(activeSession,id,name,abort.signal).then(value=>{
      if(request===generation&&!abort.signal.aborted)result=value;
    }).catch((cause:unknown)=>{
      if(request!==generation||abort.signal.aborted)return;
      if(cause instanceof APIError&&cause.status===401){onauthfailure(cause);return;}
      error=cause instanceof Error?cause.message:String(cause);
    }).finally(()=>{if(request===generation&&!abort.signal.aborted)loading=false;});
    return ()=>{generation++;abort.abort();};
  });
  function toggle(field:string,value:string,checked:boolean){
    const current=selected[field]??[];
    selected={...selected,[field]:checked?[...current,value]:current.filter(x=>x!==value)};
  }
  function suggest(field:string){
    try{onnewquery?.(qualitySuggestion(collectionID,field,selected[field]??[]));}
    catch(cause){error=cause instanceof Error?cause.message:String(cause);}
  }
</script>

<section aria-label="Collection quality" class="quality-panel">
  <div class="heading"><h3>Collection quality</h3><Button size="sm" onclick={()=>revision++}>Refresh quality</Button></div>
  {#if loading}<span role="status"><Spinner size={14}/> Loading quality…</span>{/if}
  {#if error}<p role="alert">{error}</p>{/if}
  {#if result}
    {@const coverage=result.collection.coverage}
    {#if coverage.profiles.length>0}
      <SelectDropdown title="Coverage profile" value={coverage.profile} options={coverage.profiles.map(value=>({value,label:value}))} onchange={onprofile}/>
    {/if}
    {#if coverage.configuration==="unconfigured"}<p>Processing is not configured.</p>
    {:else if coverage.configuration==="profile_required"}<p>Choose a processing profile to inspect coverage.</p>
    {:else if coverage.counts}
      <div class="counts" aria-label="Text coverage">
        <Chip>Complete: {coverage.counts.complete}</Chip><Chip>Partial: {coverage.counts.partial}</Chip>
        <Chip>Failed: {coverage.counts.failed}</Chip><Chip>Unprocessed: {coverage.counts.unprocessed}</Chip><Chip>No text: {coverage.counts.none}</Chip>
      </div>
      <p>Coverage describes retained searchable output. A configured policy does not guarantee an extraction adapter is running.</p>
    {/if}
    <p>Zero-byte documents: {result.zero_bytes}. Media/extension mismatches: {result.mismatches}. Documents with duplicate content: {result.duplicate_documents}.</p>
    {#each result.dimensions as dimension (dimension.field)}
      <details open><summary>{dimension.field}</summary>
        {#each dimension.values as bucket (bucket.value)}
          <div class="bucket">
            {#if onnewquery&&suggestible.has(dimension.field)}
              <Checkbox label={`Include ${dimension.field} ${bucket.value}`} checked={(selected[dimension.field]??[]).includes(bucket.value)} onchange={(checked)=>toggle(dimension.field,bucket.value,checked)}/>
            {:else}<span>{bucket.value}</span>{/if}
            <span>{bucket.count}</span>
          </div>
        {/each}
        <p>Missing: {dimension.missing}. Other: {dimension.other}.</p>
        {#if onnewquery&&suggestible.has(dimension.field)}<Button size="sm" disabled={!(selected[dimension.field]?.length)} onclick={()=>suggest(dimension.field)}>New query for selected {dimension.field}</Button>{/if}
      </details>
    {/each}
    {#each result.spikes as spike}<p>Concentration: {spike.field} {spike.value} ({spike.count} documents).</p>{/each}
    <p>Suggestions open a new query draft. They do not change live results or execute a search.</p>
  {/if}
</section>

<style>
  .quality-panel { display:grid; gap:var(--space-3); min-width:0; }
  .heading,.counts,.bucket { display:flex; gap:var(--space-2); align-items:center; flex-wrap:wrap; }
  .heading,.bucket { justify-content:space-between; }
  h3,p { margin:0; }
  p { color:var(--text-muted); font-size:var(--font-size-sm); }
  details { padding:var(--space-2); border:1px solid var(--border-default); border-radius:var(--radius-md); overflow-wrap:anywhere; }
</style>
