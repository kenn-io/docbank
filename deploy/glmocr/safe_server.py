"""Private, bounded HTTP boundary for the pinned GLM-OCR SDK pipeline."""

import atexit
import base64
import binascii
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


MAX_SOURCE_BYTES = 64 << 20
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
        "id": f"chatcmpl-{uuid.uuid4().hex[:29]}",
        "created": int(time.time()),
    }


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
        return jsonify({"status": "ok"})

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
