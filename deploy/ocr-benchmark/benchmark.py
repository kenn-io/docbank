#!/usr/bin/env python3
"""Benchmark the page-aware GLM-OCR endpoint without persisting OCR content."""

import argparse
import base64
import hashlib
import json
import mimetypes
import time
import urllib.request
from pathlib import Path


EXPECTED_MODEL = "glm-ocr"
DEPLOYMENT_MANIFEST = Path(__file__).resolve().parents[1] / "glmocr" / "deployment.json"


def tokens_in_order(markdown, expected):
    positions = [markdown.casefold().find(token.casefold()) for token in expected]
    return all(position >= 0 and (index == 0 or positions[index - 1] < position) for index, position in enumerate(positions))


def expected_deployment_fingerprint() -> str:
    manifest = json.loads(DEPLOYMENT_MANIFEST.read_text(encoding="utf-8"))
    encoded = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(encoded).hexdigest()


def validate_response(payload, expected_pages, deployment_fingerprint):
    if payload.get("model") != EXPECTED_MODEL:
        raise ValueError("endpoint returned the wrong model")
    if payload.get("deployment_fingerprint") != deployment_fingerprint:
        raise ValueError("endpoint returned the wrong deployment fingerprint")
    pages = payload.get("json_result")
    if (
        not isinstance(pages, list)
        or len(pages) != expected_pages
        or not all(isinstance(page, list) for page in pages)
    ):
        raise ValueError("endpoint returned the wrong page structure")
    return pages


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("fixtures", type=Path)
    parser.add_argument("--endpoint", default="http://127.0.0.1:30004/glmocr/parse")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    manifest = json.loads((args.fixtures / "manifest.json").read_text(encoding="utf-8"))
    deployment_fingerprint = expected_deployment_fingerprint()
    report = {
        "endpoint": args.endpoint,
        "model": EXPECTED_MODEL,
        "deployment_fingerprint": deployment_fingerprint,
        "cases": {},
    }
    for name, spec in manifest.items():
        path = args.fixtures / spec["file"]
        media_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        encoded = base64.b64encode(path.read_bytes()).decode("ascii")
        body = json.dumps({"images": f"data:{media_type};base64,{encoded}", "max_units": spec["pages"]}).encode()
        request = urllib.request.Request(args.endpoint, data=body, headers={"Content-Type": "application/json"})
        started = time.monotonic()
        try:
            with urllib.request.urlopen(request, timeout=600) as response:
                payload = json.load(response)
            elapsed = time.monotonic() - started
            markdown = payload.get("markdown_result", "")
            pages = validate_response(payload, spec["pages"], deployment_fingerprint)
            report["cases"][name] = {
                "seconds": round(elapsed, 3),
                "pages": len(pages),
                "functional_ok": True,
                "quality": {
                    "expected_tokens": len(spec["expected"]),
                    "matched_tokens": sum(token.casefold() in markdown.casefold() for token in spec["expected"]),
                    "ordered_tokens": tokens_in_order(markdown, spec["expected"]),
                    "markdown_table": ("|" in markdown or "<table" in markdown.casefold()) if name == "table" else None,
                    "formula_markup": any(mark in markdown for mark in ("$", "\\(", "\\[")) if name == "formula" else None,
                },
            }
        except Exception as error:  # benchmark records failure behavior by case
            report["cases"][name] = {
                "seconds": round(time.monotonic() - started, 3),
                "functional_ok": False,
                "error_type": type(error).__name__,
            }
    successful = [case for case in report["cases"].values() if case["functional_ok"]]
    report["summary"] = {
        "successful": len(successful),
        "failed": len(report["cases"]) - len(successful),
        "pages": sum(case.get("pages", 0) for case in successful),
        "seconds": round(sum(case["seconds"] for case in successful), 3),
    }
    if report["summary"]["seconds"]:
        report["summary"]["pages_per_second"] = round(report["summary"]["pages"] / report["summary"]["seconds"], 3)
    rendered = json.dumps(report, indent=2) + "\n"
    if args.output:
        args.output.write_text(rendered, encoding="utf-8")
    print(rendered, end="")


if __name__ == "__main__":
    main()
