"""Internal vLLM middleware that proves the running engine configuration."""

import hashlib
import json
import os
from functools import lru_cache
from importlib.metadata import version
from pathlib import Path

from starlette.responses import JSONResponse


DEPLOYMENT_MANIFEST = Path("/etc/glmocr/deployment.json")
ENGINE_ADAPTER = Path(__file__)
MODEL_ROOT = Path("/models/glm-ocr")
ENVIRONMENT_PREFIXES = (
    "FLASHINFER_",
    "HF_",
    "TRANSFORMERS_",
    "TRITON_",
    "VLLM_",
)


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


def validated_deployment() -> str:
    manifest = json.loads(DEPLOYMENT_MANIFEST.read_text(encoding="utf-8"))
    encoded = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    fingerprint = hashlib.sha256(encoded).hexdigest()
    if sha256_file(ENGINE_ADAPTER) != manifest["engine_adapter_sha256"]:
        raise RuntimeError("engine adapter does not match the pinned deployment")
    command = [part.decode("utf-8") for part in Path("/proc/1/cmdline").read_bytes().split(b"\0") if part]
    if command != manifest["engine_command"]:
        raise RuntimeError("engine command does not match the pinned deployment")
    environment = {
        key: value
        for key, value in os.environ.items()
        if key.startswith(ENVIRONMENT_PREFIXES)
    }
    if environment != manifest["engine_environment"]:
        raise RuntimeError("engine environment does not match the pinned deployment")
    if version("vllm") != manifest["vllm_version"]:
        raise RuntimeError("vLLM version does not match the pinned deployment")
    validate_snapshot(MODEL_ROOT, manifest["model_files"], "engine model")
    return fingerprint


@lru_cache(maxsize=1)
def deployment_status() -> tuple[str, bool]:
    try:
        return validated_deployment(), True
    except Exception:
        return "", False


async def deployment_identity(request, call_next):
    fingerprint, valid = deployment_status()
    if not valid:
        return JSONResponse({"status": "unavailable"}, status_code=503)
    if request.url.path == "/deployment":
        return JSONResponse({"deployment_fingerprint": fingerprint})
    return await call_next(request)
