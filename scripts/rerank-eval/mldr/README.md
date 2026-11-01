# MLDR English preregistered sample (2026-10-30 v1)

This recipe prepares the pinned public sample for Docbank's passage-rerank gate. It makes no provider calls, runs no retrieval, and does not score quality. Corpus text and downloads stay outside Git in an owner-private temporary directory. Do not publish the converted text.

## Fixed sample and labels

Use MLDR English test at revision `d67138e705d963e346253a80e59676ddb418810a`. Select the 100 test query IDs with the lowest SHA-256 of UTF-8 `docbank-mldr-en-test-20261030`, a NUL byte, and UTF-8 query ID; ties use ID. Freeze these IDs before scoring. Independently select 2,000 corpus documents by the same hash rule on docid, excluding every positive document from all English-test queries. The shared document pool contains all English-test positives plus those distractors. It is relevance-enriched and is not the full MLDR benchmark. There is no per-query positive injection or candidate construction in this converter.

Retain every qrel for selected queries, binarizing positive grades to 1 and zero to 0. Unjudged documents are treated as nonrelevant and judgments are incomplete. The converter cross-checks the full test split's `positive_passages` IDs against positive qrels. It fails on missing positives, duplicate IDs, missing selected judgment references or insufficient distractors. `critical=false` represents missing critical-span labels, never proof of no critical misses. MLDR's positive_passages are whole articles; answer-paragraph offsets are absent.

The converter verifies compressed corpus/test SHA-256 and qrels Git blob ID before processing. Public runs require exactly 100 queries and 2,000 distractors. The sidecar manifest freezes query/document IDs, role, text SHA-256, selection hashes, source hashes, qrels blob, converter hash and output hash. It refuses pre-existing outputs. Synthetic fixture mode bypasses source pins solely for tests and never supports the gate.

## Download and convert

Run the synthetic tests before downloading. Required User-Agent applies to all downloads.

```sh
recipe="$(pwd)/scripts/rerank-eval/mldr"
uv run python "$recipe/test_converter.py"
umask 077
mkdir -p /tmp/docbank-passage-eval-20261030/input
cd /tmp/docbank-passage-eval-20261030
revision=d67138e705d963e346253a80e59676ddb418810a
base="https://huggingface.co/datasets/Shitao/MLDR/resolve/$revision"
agent='OpenAI File Downloader, XaiImageApiFetch/1.0'
curl -fL --retry 3 -A "$agent" "$base/mldr-v1.0-en/corpus.jsonl.gz" -o input/corpus.jsonl.gz
curl -fL --retry 3 -A "$agent" "$base/mldr-v1.0-en/test.jsonl.gz" -o input/test.jsonl.gz
curl -fL --retry 3 -A "$agent" "$base/qrels/qrels.mldr-v1.0-en-test.tsv" -o input/qrels.tsv
uv run python "$recipe/convert.py" --corpus input/corpus.jsonl.gz --queries input/test.jsonl.gz --qrels input/qrels.tsv --output corpus.json
```

The compressed corpus SHA-256 is `3fe3c8473eccf8a35deda63981ac838f6a866319d3568e7472ba1a873c9e0341`; compressed test SHA-256 is `e3d79c5cb2c5c635aa9b7908e636cdbc99bb76db5ebe65cbff96acd4a10204c0`; qrels Git blob is `e5c564552e638d4f27ce7cd6c825d3b401aa00dc`.

## Attribution and source limitations

The pinned [MLDR card](https://huggingface.co/datasets/Shitao/MLDR/blob/d67138e705d963e346253a80e59676ddb418810a/README.md) declares MIT and identifies English documents as Wikipedia. The pinned [card](https://huggingface.co/datasets/Shitao/MLDR/blob/d67138e705d963e346253a80e59676ddb418810a/README.md) and [loader](https://huggingface.co/datasets/Shitao/MLDR/blob/d67138e705d963e346253a80e59676ddb418810a/MLDR.py) retain the available dataset attribution and BGE M3 citation. The upstream loader bears an Apache-2.0 notice; this recipe does not redistribute its code or the corpus. Dataset records expose opaque IDs and text, without article titles, URLs, author histories or source revisions. Preserve this available dataset-level Wikipedia attribution and disclose that article-level attribution/revisions are absent. The card's MIT label does not establish resolution of underlying Wikipedia redistribution obligations; publish only aggregate evidence and this recipe.

Citation: Jianlv Chen, Shitao Xiao, Peitian Zhang, Kun Luo, Defu Lian and Zheng Liu (2024), *BGE M3-Embedding: Multi-Lingual, Multi-Functionality, Multi-Granularity Text Embeddings Through Self-Knowledge Distillation*, [arXiv:2402.03216](https://arxiv.org/abs/2402.03216).
