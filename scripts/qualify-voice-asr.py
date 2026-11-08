#!/usr/bin/env python3
"""Remote CI only: synthetic audio -> pinned real Docling ASR -> Go adapter."""
import importlib.metadata
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

root = Path(__file__).resolve().parent.parent
output = root / ".superpowers/asr-qualification"
output.mkdir(parents=True, exist_ok=True)
subprocess.run([
    "ffmpeg", "-nostdin", "-y", "-i",
    str(root / "document/docling/testdata/asr-spoken.wav"),
    "-map_metadata", "-1", "-c:a", "aac", "-b:a", "96k",
    str(output / "asr-spoken.m4a"),
], check=True)
versions = {name: importlib.metadata.version(name) for name in (
    "docling-serve", "docling-slim", "docling-core", "docling-jobkit", "openai-whisper"
)}
(output / "versions.json").write_text(json.dumps(versions, indent=2) + "\n")
with (output / "service.log").open("w") as log:
    service = subprocess.Popen([
        str(root / ".superpowers/asr-venv/bin/docling-serve"),
        "run", "--host", "127.0.0.1", "--port", "5001",
    ], stdout=log, stderr=subprocess.STDOUT, cwd=root)
    try:
        deadline = time.monotonic() + 120
        while True:
            if service.poll() is not None:
                raise RuntimeError("isolated Docling service exited during startup")
            try:
                with urllib.request.urlopen("http://127.0.0.1:5001/health", timeout=2):
                    break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise TimeoutError("isolated Docling service did not become ready")
                time.sleep(1)
        env = dict(os.environ, DOCBANK_CI_ASR_URL="http://127.0.0.1:5001")
        subprocess.run([
            "go", "test", "-tags", "fts5", "-count=1", "-v", "-timeout", "8m",
            "./document/docling", "-run", "^TestDoclingASRRealM4AQualification$",
        ], cwd=root, env=env, check=True)
    finally:
        service.terminate()
        try:
            service.wait(timeout=20)
        except subprocess.TimeoutExpired:
            service.kill()
            service.wait()
