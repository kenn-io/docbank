#!/usr/bin/env python3
"""Convert the preregistered pinned MLDR English reduced-pool sample."""

import argparse
import gzip
import hashlib
import heapq
import json
import os
import pathlib
import tempfile
from collections import defaultdict

REV = "d67138e705d963e346253a80e59676ddb418810a"
SEED = "docbank-mldr-en-test-20261030"
QUERY_COUNT = 100
DISTRACTOR_COUNT = 2000
EXPECTED = {
    "corpus": "3fe3c8473eccf8a35deda63981ac838f6a866319d3568e7472ba1a873c9e0341",
    "queries": "e3d79c5cb2c5c635aa9b7908e636cdbc99bb76db5ebe65cbff96acd4a10204c0",
}
QRELS_BLOB = "e5c564552e638d4f27ce7cd6c825d3b401aa00dc"
USER_AGENT = "OpenAI File Downloader, XaiImageApiFetch/1.0"


def sha(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def text_sha(value):
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def rank(identifier):
    return (hashlib.sha256((SEED + "\0" + identifier).encode("utf-8")).hexdigest(), identifier)


def lines(path):
    with gzip.open(path, "rt", encoding="utf-8") as stream:
        for line in stream:
            yield json.loads(line)


def encode(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


class WorstFirst:
    """Make the heap root the worst retained (hash, ID), including hash ties."""

    def __init__(self, identifier, text):
        self.key = rank(identifier)
        self.identifier = identifier
        self.text = text

    def __lt__(self, other):
        return self.key > other.key


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("corpus", "queries", "qrels", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--count", type=int, default=QUERY_COUNT)
    parser.add_argument("--distractors", type=int, default=DISTRACTOR_COUNT)
    parser.add_argument("--synthetic-fixture", action="store_true", help="unpinned synthetic inputs only; never quality evidence")
    args = parser.parse_args()
    if args.count < 1 or args.distractors < 0:
        parser.error("count must be positive and distractors nonnegative")
    output = pathlib.Path(args.output)
    manifest_path = pathlib.Path(str(output) + ".manifest.json")
    if output.exists() or manifest_path.exists():
        raise FileExistsError("output or manifest already exists")
    hashes = {key: sha(getattr(args, key)) for key in ("corpus", "queries", "qrels")}
    qrels_bytes = pathlib.Path(args.qrels).read_bytes()
    qrels_blob = hashlib.sha1(b"blob " + str(len(qrels_bytes)).encode("ascii") + b"\0" + qrels_bytes).hexdigest()
    if not args.synthetic_fixture:
        if args.count != QUERY_COUNT or args.distractors != DISTRACTOR_COUNT:
            raise ValueError("public sample requires exactly 100 queries and 2000 distractors")
        for key, expected in EXPECTED.items():
            if hashes[key] != expected:
                raise ValueError(f"{key} does not match pinned source")
        if qrels_blob != QRELS_BLOB:
            raise ValueError("qrels does not match pinned Git blob")

    judgments = defaultdict(dict)
    for line in qrels_bytes.decode("utf-8").splitlines():
        parts = line.split()
        if len(parts) != 4 or parts[1] != "Q0":
            raise ValueError("malformed qrel")
        query, _, document, grade_text = parts
        grade = int(grade_text)
        if grade < 0 or document in judgments[query]:
            raise ValueError("invalid or duplicate qrel")
        judgments[query][document] = int(grade > 0)

    queries = {}
    query_positives = {}
    for query in lines(args.queries):
        identifier, text = query["query_id"], query["query"]
        if not isinstance(identifier, str) or not identifier or identifier in queries or not isinstance(text, str) or not text:
            raise ValueError("invalid or duplicate query")
        queries[identifier] = text
        query_positives[identifier] = {item["docid"] for item in query.get("positive_passages", [])}
    if set(judgments) - set(queries):
        raise ValueError("qrel references missing query")
    if len(queries) < args.count:
        raise ValueError("not enough test queries")
    # Cross-check both published sources; positives from ALL test queries form the pool.
    for identifier in queries:
        if query_positives[identifier] != {doc for doc, grade in judgments[identifier].items() if grade > 0}:
            raise ValueError("test positive_passages disagree with positive qrels")
    positives = set().union(*query_positives.values())
    chosen = sorted(sorted(queries, key=rank)[: args.count])
    documents = {}
    distractors = []
    seen = set()
    for document in lines(args.corpus):
        identifier, text = document["docid"], document["text"]
        if not isinstance(identifier, str) or not identifier or identifier in seen or not isinstance(text, str) or not text:
            raise ValueError("invalid or duplicate corpus document")
        seen.add(identifier)
        if identifier in positives:
            documents[identifier] = text
        elif args.distractors:
            item = WorstFirst(identifier, text)
            if len(distractors) < args.distractors:
                heapq.heappush(distractors, item)
            elif item.key < distractors[0].key:
                heapq.heapreplace(distractors, item)
    if positives - set(documents):
        raise ValueError("test positives reference missing corpus documents")
    if len(distractors) != args.distractors:
        raise ValueError("not enough nonpositive distractors")
    distractor_ids = {item.identifier for item in distractors}
    documents.update({item.identifier: item.text for item in distractors})
    needed = {doc for query in chosen for doc in judgments[query]}
    if needed - set(documents):
        raise ValueError("selected full qrels reference documents outside the preregistered pool")
    result = {
        "id": "mldr-en-test-reduced-pool-20261030",
        "version": REV,
        "documents": [{"id": identifier, "text": documents[identifier]} for identifier in sorted(documents)],
        "queries": [{"id": identifier, "text": queries[identifier], "judgments": [{"document_id": doc, "grade": grade, "critical": False} for doc, grade in sorted(judgments[identifier].items())]} for identifier in chosen],
    }
    manifest = {
        "dataset": "Shitao/MLDR", "revision": REV, "split": "en-test",
        "synthetic_fixture": args.synthetic_fixture,
        "source": "https://huggingface.co/datasets/Shitao/MLDR/tree/" + REV,
        "source_urls": {"corpus": f"https://huggingface.co/datasets/Shitao/MLDR/resolve/{REV}/mldr-v1.0-en/corpus.jsonl.gz", "queries": f"https://huggingface.co/datasets/Shitao/MLDR/resolve/{REV}/mldr-v1.0-en/test.jsonl.gz", "qrels": f"https://huggingface.co/datasets/Shitao/MLDR/resolve/{REV}/qrels/qrels.mldr-v1.0-en-test.tsv"},
        "download_user_agent": USER_AGENT,
        "dataset_license_card": "MIT", "loader_license": "Apache-2.0",
        "underlying_source": "English Wikipedia; article-level titles, URLs, histories and revisions absent",
        "attribution": "MLDR by Shitao Xiao et al.; BGE M3-Embedding, Chen et al. (2024), arXiv:2402.03216; English source Wikipedia",
        "sampling": "100 lowest SHA-256(seed UTF-8 + NUL + query_id UTF-8), tie ID; all English-test positive documents plus independently selected 2000 lowest SHA-256(seed UTF-8 + NUL + docid UTF-8), excluding all test positives; emit sorted IDs",
        "seed": SEED, "test_query_count": len(queries), "source_document_count": len(seen),
        "query_count": len(chosen), "query_ids": chosen,
        "query_manifest": [{"id": identifier, "selection_sha256": rank(identifier)[0], "text_sha256": text_sha(queries[identifier])} for identifier in chosen],
        "document_count": len(documents), "all_test_positive_document_count": len(positives),
        "all_test_positive_document_ids": sorted(positives), "distractor_count": len(distractor_ids),
        "distractor_document_ids": sorted(distractor_ids),
        "document_manifest": [{"id": identifier, "role": "test_positive" if identifier in positives else "distractor", "selection_sha256": rank(identifier)[0], "text_sha256": text_sha(documents[identifier]), "utf8_bytes": len(documents[identifier].encode("utf-8"))} for identifier in sorted(documents)],
        "judgment_count": sum(len(judgments[query]) for query in chosen),
        "all_test_judgment_count": sum(len(value) for value in judgments.values()),
        "judgment_policy": "full selected-query qrels, grade > 0 becomes 1; unjudged treated as nonrelevant; judgments incomplete; no per-query positive injection",
        "critical": "false records unavailable critical-span annotations, never absence of critical misses",
        "scope": "relevance-enriched reduced pool, not the full MLDR benchmark; query failures and zero-positive retrieval remain in evaluation",
        "input_sha256": hashes, "qrels_git_blob": qrels_blob,
        "converter_sha256": sha(__file__),
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".mldr-", dir=output.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
            stream.write(encode(result) + "\n")
        manifest["output_sha256"] = sha(temporary)
        # Outputs are immutable per run; refuse pre-existing paths again before install.
        with output.open("xb") as destination, open(temporary, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                destination.write(chunk)
        with manifest_path.open("x", encoding="utf-8", newline="\n") as stream:
            stream.write(encode(manifest) + "\n")
    finally:
        os.unlink(temporary)
    print(encode({"output": str(output), "manifest": str(manifest_path), "document_count": len(documents), "query_count": len(chosen), "output_sha256": manifest["output_sha256"]}))


if __name__ == "__main__":
    main()
