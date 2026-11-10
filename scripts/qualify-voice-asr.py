#!/usr/bin/env python3
"""Remote CI only: synthetic audio -> pinned real Docling ASR -> Go adapter."""
import importlib.metadata
import hashlib
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
# Synthetic speech only. Provider-host FFmpeg generates inputs; Docbank never demuxes.
for name, duration, bitrate, loops in (
    ("asr-spoken.mp4", "6", "1M", 0),
    ("asr-large.mp4", "25", "16M", 0),
    ("asr-multisegment.mp4", "40", "1M", 12),
):
    subprocess.run([
        "ffmpeg", "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
        "-stream_loop", str(loops), "-i", str(root / "document/docling/testdata/asr-spoken.wav"),
        "-t", duration, "-map_metadata", "-1", "-c:v", "libx264", "-preset", "ultrafast",
        "-b:v", bitrate, "-minrate", bitrate, "-maxrate", bitrate, "-bufsize", "32M",
        "-x264-params", "nal-hrd=cbr", "-c:a", "aac", "-af", "apad", str(output / name),
    ], check=True)
assert (output / "asr-large.mp4").stat().st_size > 38 << 20
versions = {name: importlib.metadata.version(name) for name in (
    "docling-serve", "docling-slim", "docling-core", "docling-jobkit", "openai-whisper"
)}
expected_versions = {"docling-serve": "1.32.0", "docling-slim": "2.124.0",
                     "docling-core": "2.105.0", "docling-jobkit": "3.5.0",
                     "openai-whisper": "20250625"}
if versions != expected_versions:
    raise RuntimeError(f"unqualified provider versions: {versions}")
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
            "./document/docling", "-run", "^TestDoclingASRRealAudioQualification$",
        ], cwd=root, env=env, check=True)
        source = (output / "asr-spoken.m4a").read_bytes()
        transcript = (output / "media-transcript.json").read_bytes()
        probe = subprocess.run([
            "ffprobe", "-v", "error", "-select_streams", "a:0",
            "-show_entries", "stream=codec_name,sample_rate,channels:format=duration,format_name",
            "-of", "json", str(output / "asr-spoken.m4a"),
        ], check=True, capture_output=True, text=True)
        report = {
            "contract": "docbank-asr-qualification/v1",
            "source": "synthetic asr-spoken.wav transcoded to AAC/M4A",
            "source_sha256": hashlib.sha256(source).hexdigest(),
            "artifact_sha256": hashlib.sha256(transcript).hexdigest(),
            "mp3_artifact_sha256": hashlib.sha256((output / "mp3-transcript.json").read_bytes()).hexdigest(),
            "versions": versions,
            "decoder": json.loads(probe.stdout),
            "adapter": "docbank-docling-asr/v3",
            "accuracy": "not qualified",
            "videos": {name: {
                "source_bytes": (output / name).stat().st_size,
                "decoder": json.loads(subprocess.run(["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", str(output / name)], check=True, capture_output=True, text=True).stdout),
                "adapter_measurement": json.loads((output / (name + "-transcript.json.measurement.json")).read_text()),
            } for name in ("asr-spoken.mp4", "asr-large.mp4", "asr-multisegment.mp4")},
            "document_schema": "1.10.0",
            "expected_terms": ["delivery", "arrives", "friday"],
            "expected_synthetic_phrase": "Telescope delivery arrives Friday at 3.",
            "accuracy_note": "ASR may misrecognize words; this qualification verifies decoding and timed speech, not verbatim accuracy",
        }
        (output / "qualification.json").write_text(json.dumps(report, indent=2) + "\n")
    finally:
        service.terminate()
        try:
            service.wait(timeout=20)
        except subprocess.TimeoutExpired:
            service.kill()
            service.wait()
