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
from importlib.metadata import distribution, version
from io import BytesIO
from pathlib import Path

from flask import Flask, jsonify, request
from glmocr.config import load_config
from glmocr.pipeline import Pipeline
from glmocr.utils.logging import configure_logging
from PIL import Image, UnidentifiedImageError
import pymupdf


MAX_SOURCE_BYTES = 64 << 20
DEPLOYMENT_MANIFEST = Path("/etc/glmocr/deployment.json")
ADAPTER_PATH = Path(__file__)
IMAGE_RECIPE_PATH = Path("/etc/glmocr/Dockerfile")
DEPENDENCY_LOCK_PATH = Path("/etc/glmocr/requirements.lock")
MODEL_ROOT = Path("/models/glm-ocr")
LAYOUT_ROOT = Path("/models/pp-doclayout-v3")
MEDIA_SUFFIXES = {
    "application/pdf": ".pdf",
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/webp": ".webp",
}
PYMUPDF_INPUT_ERRORS = tuple(
    error
    for name in ("EmptyFileError", "FileDataError")
    if (error := getattr(pymupdf, name, None)) is not None
)


class InvalidDocument(ValueError):
    """The decoded source cannot be parsed as its declared document type."""


def response_payload(json_result, markdown_result, deployment_fingerprint):
    return {
        "json_result": json_result,
        "markdown_result": markdown_result,
        "layout_details": json_result,
        "md_results": markdown_result,
        "data_info": {"pages": []},
        "usage": {},
        "model": "glm-ocr",
        "deployment_fingerprint": deployment_fingerprint,
        "id": f"chatcmpl-{uuid.uuid4().hex[:29]}",
        "created": int(time.time()),
    }


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def artifact_digest(path: Path, algorithm: str) -> str:
    if algorithm == "sha256":
        return sha256_file(path)
    if algorithm == "git-sha1":
        content = path.read_bytes()
        digest = hashlib.sha1(usedforsecurity=False)
        digest.update(f"blob {len(content)}\0".encode())
        digest.update(content)
        return digest.hexdigest()
    raise RuntimeError("deployment manifest uses an unsupported digest")


def validate_snapshot(root: Path, artifacts: object, name: str) -> None:
    if not isinstance(artifacts, list) or not artifacts:
        raise RuntimeError(f"{name} manifest is empty")
    expected_paths = set()
    for artifact in artifacts:
        if not isinstance(artifact, dict) or set(artifact) != {"path", "algorithm", "digest"}:
            raise RuntimeError(f"{name} manifest entry is invalid")
        relative = artifact["path"]
        if not isinstance(relative, str) or not relative or Path(relative).is_absolute() or ".." in Path(relative).parts:
            raise RuntimeError(f"{name} manifest path is invalid")
        if relative in expected_paths:
            raise RuntimeError(f"{name} manifest path is duplicated")
        expected_paths.add(relative)
        candidate = root / relative
        if not candidate.is_file() or artifact_digest(candidate, artifact["algorithm"]) != artifact["digest"]:
            raise RuntimeError(f"{name} artifact {relative} does not match the pinned deployment")
    actual_paths = {
        str(path.relative_to(root))
        for path in root.rglob("*")
        if path.is_file() and ".cache" not in path.relative_to(root).parts
    }
    if actual_paths != expected_paths:
        raise RuntimeError(f"{name} snapshot does not match the complete manifest")


def validate_deployment(config_path: str) -> str:
    manifest = json.loads(DEPLOYMENT_MANIFEST.read_text(encoding="utf-8"))
    if not isinstance(manifest, dict):
        raise RuntimeError("deployment manifest is invalid")
    encoded = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    deployment_fingerprint = hashlib.sha256(encoded).hexdigest()
    checks = {
        "pipeline configuration": (sha256_file(Path(config_path)), manifest["pipeline_config_sha256"]),
        "service adapter": (sha256_file(ADAPTER_PATH), manifest["adapter_sha256"]),
        "image recipe": (sha256_file(IMAGE_RECIPE_PATH), manifest["image_recipe_sha256"]),
        "dependency lock": (sha256_file(DEPENDENCY_LOCK_PATH), manifest["dependency_lock_sha256"]),
        "PyMuPDF version": (pymupdf.pymupdf_version, manifest["pymupdf_version"]),
        "MuPDF version": (pymupdf.mupdf_version, manifest["mupdf_version"]),
    }
    for name, (actual, wanted) in checks.items():
        if actual != wanted:
            raise RuntimeError(f"{name} does not match the pinned deployment")
    dependencies = manifest.get("runtime_dependencies")
    if not isinstance(dependencies, list) or not dependencies:
        raise RuntimeError("runtime dependency manifest is invalid")
    for dependency in dependencies:
        if not isinstance(dependency, dict) or set(dependency) != {"name", "version"}:
            raise RuntimeError("runtime dependency manifest entry is invalid")
        if version(dependency["name"]) != dependency["version"]:
            raise RuntimeError(f"runtime dependency {dependency['name']} does not match the pinned deployment")
    direct_url = json.loads(distribution("glmocr").read_text("direct_url.json"))
    expected_sdk_url = f"https://github.com/zai-org/GLM-OCR/archive/{manifest['sdk_revision']}.zip"
    if direct_url.get("url") != expected_sdk_url:
        raise RuntimeError("GLM-OCR SDK does not match the pinned deployment")
    validate_snapshot(MODEL_ROOT, manifest["model_files"], "GLM-OCR")
    validate_snapshot(LAYOUT_ROOT, manifest["layout_files"], "layout model")
    return deployment_fingerprint


def validate_engine(deployment_fingerprint: str) -> None:
    with urllib.request.urlopen("http://engine:30005/deployment", timeout=2) as upstream:
        if upstream.status != 200:
            raise RuntimeError("engine deployment is not ready")
        body = upstream.read(4097)
        if len(body) > 4096:
            raise RuntimeError("engine deployment response is too large")
    identity = json.loads(body)
    if not isinstance(identity, dict) or set(identity) != {"deployment_fingerprint"}:
        raise RuntimeError("engine deployment response is invalid")
    if identity["deployment_fingerprint"] != deployment_fingerprint:
        raise RuntimeError("engine deployment fingerprint does not match the adapter")


def validate_document(content: bytes, suffix: str, pdf_max_pages: int) -> int:
    try:
        if suffix == ".pdf":
            with pymupdf.open(stream=content, filetype="pdf") as document:
                if document.needs_pass or document.page_count <= 0:
                    raise InvalidDocument("PDF cannot be opened for rendering")
                if document.page_count > pdf_max_pages:
                    raise InvalidDocument("PDF exceeds the configured page limit")
                for page_index in range(document.page_count):
                    page = document.load_page(page_index)
                    page.get_pixmap(matrix=pymupdf.Matrix(0.1, 0.1), alpha=False)
                return document.page_count
        with Image.open(BytesIO(content)) as image:
            image.verify()
        with Image.open(BytesIO(content)) as image:
            image.load()
        return 1
    except InvalidDocument:
        raise
    except PYMUPDF_INPUT_ERRORS + (UnidentifiedImageError, OSError, RuntimeError, SyntaxError, ValueError) as error:
        raise InvalidDocument("source cannot be decoded") from error


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
    deployment_fingerprint = validate_deployment(config_path)
    config = load_config(config_path)
    pdf_max_pages = config.pipeline.page_loader.pdf_max_pages
    configure_logging(level=config.logging.level)
    pipeline = Pipeline(config=config.pipeline)
    pipeline.start()
    atexit.register(pipeline.stop)

    app = Flask(__name__)
    app.config["MAX_CONTENT_LENGTH"] = 90 << 20

    @app.get("/health")
    def health():
        try:
            validate_engine(deployment_fingerprint)
        except Exception:
            return jsonify({"status": "unavailable"}), 503
        return jsonify({"status": "ok", "deployment_fingerprint": deployment_fingerprint})

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
        try:
            source_units = validate_document(content, suffix, pdf_max_pages)
        except InvalidDocument:
            return jsonify({"error": "source cannot be decoded"}), 422
        try:
            validate_engine(deployment_fingerprint)
        except Exception:
            return jsonify({"error": "OCR engine is unavailable"}), 503

        temp_path = None
        try:
            with tempfile.NamedTemporaryFile(prefix="docbank-ocr-", suffix=suffix, delete=False) as staged:
                temp_path = staged.name
                staged.write(content)
            os.chmod(temp_path, 0o600)
            messages = [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": temp_path}}]}]
            results = list(pipeline.process({"messages": messages}, save_layout_visualization=False))
            if len(results) != 1:
                return jsonify({"error": "OCR pipeline returned no document"}), 422
            result = results[0]
            pages = result.json_result
            if not isinstance(pages, list) or not pages:
                return jsonify({"error": "OCR pipeline returned no evidence"}), 422
            if len(pages) != source_units:
                return jsonify({"error": "OCR pipeline returned the wrong page count"}), 502
            if not all(isinstance(page, list) for page in pages):
                return jsonify({"error": "OCR pipeline returned malformed pages"}), 502
            if not any(pages):
                return jsonify({"error": "OCR pipeline returned no evidence"}), 422
            return jsonify(response_payload(pages, result.markdown_result or "", deployment_fingerprint))
        except Exception as error:
            app.logger.error("OCR pipeline failed: %s", type(error).__name__)
            return jsonify({"error": "OCR pipeline failed"}), 502
        finally:
            if temp_path:
                Path(temp_path).unlink(missing_ok=True)

    return app
