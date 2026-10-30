"""Synthetic-only regression checks; no downloaded text or provider calls."""
import gzip
import hashlib
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("convert.py")
SEED = "docbank-mldr-en-test-20261030"


def rank(identifier):
    return hashlib.sha256((SEED + "\0" + identifier).encode()).hexdigest(), identifier


class ConverterTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.documents = [{"docid": f"d{i}", "text": f"Synthetic article {i}: Béta 🚀"} for i in range(16)]
        self.queries = [{"query_id": f"q{i}", "query": f"Question {i}?", "positive_passages": [{"docid": f"d{i}", "text": self.documents[i]["text"]}]} for i in range(5)]
        self.write_gzip("corpus.gz", self.documents)
        self.write_gzip("queries.gz", self.queries)
        self.qrels = "".join(f"q{i} Q0 d{i} {2 if i == 0 else 1}\n" for i in range(5))
        (self.root / "qrels").write_text(self.qrels)

    def write_gzip(self, name, rows):
        with gzip.open(self.root / name, "wt", encoding="utf-8") as stream:
            for row in rows:
                stream.write(json.dumps(row) + "\n")

    def run_converter(self, name, count=2, distractors=3, fixture=True):
        args = [sys.executable, str(SCRIPT), "--corpus", str(self.root / "corpus.gz"), "--queries", str(self.root / "queries.gz"), "--qrels", str(self.root / "qrels"), "--output", str(self.root / name), "--count", str(count), "--distractors", str(distractors)]
        if fixture:
            args.append("--synthetic-fixture")
        return subprocess.run(args, capture_output=True, text=True)

    def test_sample_full_pool_and_determinism(self):
        self.assertEqual(self.run_converter("one").returncode, 0)
        self.assertEqual(self.run_converter("two").returncode, 0)
        self.assertEqual((self.root / "one").read_bytes(), (self.root / "two").read_bytes())
        self.assertEqual((self.root / "one.manifest.json").read_bytes(), (self.root / "two.manifest.json").read_bytes())
        result = json.loads((self.root / "one").read_text())
        manifest = json.loads((self.root / "one.manifest.json").read_text())
        expected_queries = sorted(sorted([f"q{i}" for i in range(5)], key=rank)[:2])
        expected_distractors = sorted(sorted([f"d{i}" for i in range(5, 16)], key=rank)[:3])
        self.assertEqual([query["id"] for query in result["queries"]], expected_queries)
        self.assertEqual(manifest["distractor_document_ids"], expected_distractors)
        self.assertEqual(manifest["all_test_positive_document_ids"], [f"d{i}" for i in range(5)])
        self.assertEqual(len(result["documents"]), 8)
        for query in result["queries"]:
            self.assertEqual(query["judgments"], [{"document_id": "d" + query["id"][1:], "grade": 1, "critical": False}])
        self.assertEqual(manifest["output_sha256"], hashlib.sha256((self.root / "one").read_bytes()).hexdigest())
        self.assertNotEqual(self.run_converter("one").returncode, 0)

    def test_full_multi_qrels_binary(self):
        chosen = sorted([f"q{i}" for i in range(5)], key=rank)[0]
        extra = "d0" if chosen != "q0" else "d1"
        self.queries[int(chosen[1:])]["positive_passages"].append({"docid": extra, "text": "Synthetic second positive"})
        self.write_gzip("queries.gz", self.queries)
        (self.root / "qrels").write_text(self.qrels + f"{chosen} Q0 {extra} 2\n")
        self.assertEqual(self.run_converter("multi", count=1).returncode, 0)
        result = json.loads((self.root / "multi").read_text())
        self.assertEqual(len(result["queries"][0]["judgments"]), 2)
        self.assertTrue(all(item["grade"] == 1 for item in result["queries"][0]["judgments"]))

    def test_reject_missing_positive_even_unselected(self):
        self.write_gzip("corpus.gz", self.documents[1:])
        self.assertNotEqual(self.run_converter("missing").returncode, 0)
        self.assertFalse((self.root / "missing").exists())

    def test_source_pins_and_exact_sizes_enforced(self):
        public = self.run_converter("public", fixture=False, count=100, distractors=2000)
        self.assertNotEqual(public.returncode, 0)
        self.assertIn("corpus does not match pinned source", public.stderr)
        self.assertFalse((self.root / "public").exists())
        self.assertNotEqual(self.run_converter("count", count=6).returncode, 0)
        self.assertNotEqual(self.run_converter("distractors", distractors=30).returncode, 0)

    def test_duplicate_and_disagreement_rejected(self):
        self.write_gzip("corpus.gz", self.documents + self.documents[:1])
        self.assertNotEqual(self.run_converter("duplicate").returncode, 0)
        self.write_gzip("corpus.gz", self.documents)
        (self.root / "qrels").write_text(self.qrels.replace("q0 Q0 d0 2", "q0 Q0 d1 2"))
        self.assertNotEqual(self.run_converter("disagree").returncode, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
