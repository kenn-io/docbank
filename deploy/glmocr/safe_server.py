"""Private, bounded HTTP boundary for the pinned GLM-OCR SDK pipeline."""

import atexit
import base64
import binascii
import hashlib
import json
import os
import tempfile
import time
import urllib.request
import uuid
from pathlib import Path

from flask import Flask, jsonify, request
from glmocr.config import load_config
from glmocr.pipeline import Pipeline
from glmocr.utils.logging import configure_logging
import pymupdf


MAX_SOURCE_BYTES = 64 << 20
DEPLOYMENT_FINGERPRINT = "d132e09cf91629e1514a75077e7d9cd7b6cc3184c96e7512b91a3cb5d2e8315b"
DEPLOYMENT_IDENTITY = {
    "version": 1,
    "model": "zai-org/GLM-OCR",
    "model_revision": "ca5d8b3e287e52589e37c28385d9655ee4372f9d",
    "model_sha256": "a16eb0de98d199293371c560f95f83130d2a2c9612449df16839f08ff9498815",
    "sdk_revision": "cef4d0ea120d1741f5cefe8985eee45f6c8eff1d",
    "layout_model": "PaddlePaddle/PP-DocLayoutV3_safetensors",
    "layout_revision": "97d101e6db2642e162a1d05392d1b0231c91033e",
    "layout_sha256": "5ea422c6cc5fe759a47e1357c35639b58173508e025a3131cbe4b6ac59e2b85e",
    "engine_image": "vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0",
    "pipeline_config_sha256": "f299e93f6f928640d4aa7faceb79ed24c978f71ca33195a36dd8bc9f4855c5b0",
    "pymupdf_version": "1.27.2.3",
    "mupdf_version": "1.27.2",
}
MODEL_WEIGHTS = Path("/models/glm-ocr/model.safetensors")
LAYOUT_WEIGHTS = Path("/models/pp-doclayout-v3/model.safetensors")
MEDIA_SUFFIXES = {
    "application/pdf": ".pdf",
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/webp": ".webp",
}


def response_payload(json_result, markdown_result):
    return {
        "json_result": json_result,
        "markdown_result": markdown_result,
        "layout_details": json_result,
        "md_results": markdown_result,
        "data_info": {"pages": []},
        "usage": {},
        "model": "glm-ocr",
        "deployment_fingerprint": DEPLOYMENT_FINGERPRINT,
        "id": f"chatcmpl-{uuid.uuid4().hex[:29]}",
        "created": int(time.time()),
    }


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def validate_deployment(config_path: str) -> None:
    encoded = json.dumps(DEPLOYMENT_IDENTITY, separators=(",", ":")).encode()
    if hashlib.sha256(encoded).hexdigest() != DEPLOYMENT_FINGERPRINT:
        raise RuntimeError("deployment identity fingerprint is invalid")
    expected = DEPLOYMENT_IDENTITY
    checks = {
        "pipeline configuration": (sha256_file(Path(config_path)), expected["pipeline_config_sha256"]),
        "GLM-OCR weights": (sha256_file(MODEL_WEIGHTS), expected["model_sha256"]),
        "layout weights": (sha256_file(LAYOUT_WEIGHTS), expected["layout_sha256"]),
        "PyMuPDF version": (pymupdf.pymupdf_version, expected["pymupdf_version"]),
        "MuPDF version": (pymupdf.mupdf_version, expected["mupdf_version"]),
    }
    for name, (actual, wanted) in checks.items():
        if actual != wanted:
            raise RuntimeError(f"{name} does not match the pinned deployment")


def decode_source(value: object) -> tuple[bytes, str]:
    if not isinstance(value, str) or not value.startswith("data:"):
        raise ValueError("source must be a data URI")
    header, separator, encoded = value.partition(",")
    if not separator or not header.endswith(";base64"):
        raise ValueError("source must use base64 encoding")
    media_type = header[5:-7]
    suffix = MEDIA_SUFFIXES.get(media_type)
    if not suffix:
        raise ValueError("unsupported media type")
    if len(encoded) > ((MAX_SOURCE_BYTES + 2) // 3) * 4:
        raise ValueError("source exceeds size limit")
    try:
        content = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as error:
        raise ValueError("invalid base64 source") from error
    if not content or len(content) > MAX_SOURCE_BYTES:
        raise ValueError("source size is invalid")
    return content, suffix


def create_app() -> Flask:
    config_path = os.environ.get("GLMOCR_CONFIG", "/etc/glmocr/config.yaml")
    validate_deployment(config_path)
    config = load_config(config_path)
    configure_logging(level=config.logging.level)
    pipeline = Pipeline(config=config.pipeline)
    pipeline.start()
    atexit.register(pipeline.stop)

    app = Flask(__name__)
    app.config["MAX_CONTENT_LENGTH"] = 90 << 20

    @app.get("/health")
    def health():
        try:
            with urllib.request.urlopen("http://engine:30005/health", timeout=2) as upstream:
                if upstream.status != 200:
                    raise RuntimeError("engine is not ready")
        except Exception:
            return jsonify({"status": "unavailable"}), 503
        return jsonify({"status": "ok", "deployment_fingerprint": DEPLOYMENT_FINGERPRINT})

    @app.post("/glmocr/parse")
    def parse():
        if not request.is_json:
            return jsonify({"error": "content type must be application/json"}), 400
        data = request.get_json(silent=True)
        if not isinstance(data, dict):
            return jsonify({"error": "invalid JSON payload"}), 400
        source = data.get("images", data.get("file"))
        if isinstance(source, list):
            if len(source) != 1:
                return jsonify({"error": "exactly one document is required"}), 400
            source = source[0]
        try:
            content, suffix = decode_source(source)
        except ValueError as error:
            return jsonify({"error": str(error)}), 400

        temp_path = None
        try:
            with tempfile.NamedTemporaryFile(prefix="docbank-ocr-", suffix=suffix, delete=False) as staged:
                staged.write(content)
                temp_path = staged.name
            os.chmod(temp_path, 0o600)
            messages = [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": temp_path}}]}]
            results = list(pipeline.process({"messages": messages}, save_layout_visualization=False))
            if len(results) != 1:
                return jsonify({"error": "OCR pipeline returned no document"}), 502
            result = results[0]
            pages = result.json_result
            if not isinstance(pages, list) or not pages or not any(isinstance(page, list) and page for page in pages):
                return jsonify({"error": "OCR pipeline returned no evidence"}), 502
            return jsonify(response_payload(pages, result.markdown_result or ""))
        except Exception as error:
            app.logger.error("OCR pipeline failed: %s", type(error).__name__)
            return jsonify({"error": "OCR pipeline failed"}), 502
        finally:
            if temp_path:
                Path(temp_path).unlink(missing_ok=True)

    return app
