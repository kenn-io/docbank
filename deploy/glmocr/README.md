# Local GLM-OCR deployment

This deployment runs the complete GLM-OCR document pipeline on one NVIDIA
GB10 host. An internal vLLM engine recognizes regions; the GLM-OCR SDK performs
PDF rendering, layout detection, ordered region OCR, and Markdown formatting.
Only the page-aware SDK endpoint is published, on loopback port `30004`.

## Pinned artifacts and licenses

| Artifact | Immutable reference | License |
| --- | --- | --- |
| GLM-OCR model | `zai-org/GLM-OCR@ca5d8b3e287e52589e37c28385d9655ee4372f9d` | MIT |
| GLM-OCR SDK | `zai-org/GLM-OCR@cef4d0ea120d1741f5cefe8985eee45f6c8eff1d` | Apache-2.0 |
| PP-DocLayoutV3 | `PaddlePaddle/PP-DocLayoutV3_safetensors@97d101e6db2642e162a1d05392d1b0231c91033e` | Apache-2.0 |
| vLLM ARM64 image | `vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0` | Apache-2.0 |

The container build installs the SDK from its Git commit and pins every package
that it adds to the immutable vLLM base, including Transformers `5.15.1`,
safetensors `0.8.0`, and Gunicorn `23.0.0`. The newer Transformers build is
required for PP-DocLayoutV3 and GLM-OCR architecture support. The build imports
the SDK, PyTorch, and Transformers before publishing the image.

`safe_server.py` is the public service boundary. It accepts one bounded PDF or
image data URI, decodes it into a mode-`0600` file on the container's private
tmpfs, deletes it after parsing, and never logs request data. It reports backend
failures and empty recognition as errors instead of returning an empty success.

## Install

Choose an owner-private model directory outside a source checkout. These
commands download only the two pinned public model repositories:

```bash
umask 077
export GLMOCR_MODEL_ROOT=/var/lib/docbank-glmocr/models
sudo install -d -m 0700 -o "$USER" -g "$USER" "$GLMOCR_MODEL_ROOT"

docker run --rm \
  -v "$GLMOCR_MODEL_ROOT:/models" \
  --entrypoint hf \
  vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0 \
  download zai-org/GLM-OCR \
  --revision ca5d8b3e287e52589e37c28385d9655ee4372f9d \
  --local-dir /models/glm-ocr

docker run --rm \
  -v "$GLMOCR_MODEL_ROOT:/models" \
  --entrypoint hf \
  vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0 \
  download PaddlePaddle/PP-DocLayoutV3_safetensors \
  --revision 97d101e6db2642e162a1d05392d1b0231c91033e \
  --local-dir /models/pp-doclayout-v3
```

Copy this directory to `/opt/docbank-glmocr`, build it, and validate the
effective Compose configuration:

```bash
sudo install -d -m 0755 /opt/docbank-glmocr
sudo cp Dockerfile compose.yaml config.yaml safe_server.py /opt/docbank-glmocr/
sudo docker compose --project-directory /opt/docbank-glmocr build
GLMOCR_MODEL_ROOT=/var/lib/docbank-glmocr/models \
  sudo --preserve-env=GLMOCR_MODEL_ROOT \
  docker compose --project-directory /opt/docbank-glmocr config --quiet
```

Install the lifecycle unit and its non-secret environment file:

```bash
echo 'GLMOCR_MODEL_ROOT=/var/lib/docbank-glmocr/models' | \
  sudo tee /etc/docbank-glmocr.env >/dev/null
sudo chmod 0600 /etc/docbank-glmocr.env
sudo cp docbank-glmocr.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now docbank-glmocr.service
```

The service has no hosted-provider credentials. Both containers run with a
read-only root filesystem and no added privileges. The model engine is visible
only to the private Compose network. The document endpoint is bound to
`127.0.0.1:30004`; do not change it to a wildcard address without adding an
authenticated reverse proxy and a new privacy review.

## Verify

Health confirms that the page-aware pipeline is ready:

```bash
curl --fail --silent http://127.0.0.1:30004/health
```

A real smoke test must submit a synthetic image or PDF as a data URI and check
that `model` is `glm-ocr` and `json_result` contains ordered page arrays. A
successful `/health` response alone does not prove that inference works.

Verify GPU execution while that smoke test is active:

```bash
nvidia-smi pmon -c 1
docker stats --no-stream docbank-glmocr-engine-1 docbank-glmocr-api-1
```

The vLLM engine reserves at most 12 percent of unified GPU memory and accepts
four concurrent sequences. The SDK uses four region workers and a single-page
layout batch. Triton and the layout model need executable private tmpfs caches
for JIT-compiled GPU kernels; the rest of each container filesystem is
read-only.

## GB10 measurements and synthetic benchmark

Measured with the repository's synthetic fixtures on an NVIDIA DGX Spark-class
GB10 system:

| Process | Container RSS | NVIDIA reported memory |
| --- | ---: | ---: |
| vLLM engine | 4.36 GiB | 12,901 MiB |
| layout/API | 2.06 GiB | 1,279 MiB |

The downloaded GLM-OCR snapshot uses 2.5 GiB on disk; PP-DocLayoutV3 uses
128 MiB. Their primary safetensors SHA-256 values are
`a16eb0de98d199293371c560f95f83130d2a2c9612449df16839f08ff9498815`
and `5ea422c6cc5fe759a47e1357c35639b58173508e025a3131cbe4b6ac59e2b85e`.

The repository benchmark generator creates only synthetic data. A warm run
processed nine pages across eight cases in 3.878 seconds (2.321 pages/second):

| Case | Result |
| --- | --- |
| ordinary text | 2/2 markers, ordered |
| multi-column | 4/4 markers in expected reading order |
| table | 2/2 markers; structured HTML table |
| formula | formula markup present; 1/2 literal markers |
| multilingual | 3/3 English, Spanish, French, and Chinese markers |
| poor scan | 2/2 markers |
| handwriting-style | 2/2 markers |
| two-page PDF | two page arrays and 2/2 markers |

Generate and run the same private, disposable benchmark outside the checkout:

```bash
python3 deploy/ocr-benchmark/generate_fixtures.py /tmp/docbank-ocr-fixtures
python3 deploy/ocr-benchmark/benchmark.py /tmp/docbank-ocr-fixtures
```

The formula case is the clearest known quality limitation: the service emitted
LaTeX-style markup but separated the synthetic `sqrt` lettering and distorted
part of the Euler expression. The fixture uses a script font to approximate
handwriting; it is not a representative handwritten corpus. No comparison to
Mistral quality has been made.

PaddleOCR-VL-1.5 was evaluated as the comparison candidate. Its Apache-2.0
model and complete two-stage pipeline are suitable in principle, and Paddle's
Blackwell guide provides an SM120 image. A benchmark was not practical in this
run because the official Baidu container registry timed out while resolving
the versioned SM120 image, before any model or document was sent. Do not infer
relative quality from the missing Paddle result.

## Upgrade and rollback

Treat model, SDK, layout, and container revisions as one output identity.
Update all four pins in a branch, build a newly tagged image, run the synthetic
benchmark, and only then change the systemd deployment.

Rollback does not modify any other model or service:

```bash
sudo systemctl disable --now docbank-glmocr.service
sudo docker compose --project-directory /opt/docbank-glmocr down
```

Restore the previous `/opt/docbank-glmocr` files and model directories, then
enable the unit again. Keep old immutable model directories until the new
deployment passes health and real OCR smoke tests. Removing this deployment is
limited to its two containers, image tag, unit, and owner-private model root;
never prune the shared Docker or model stores as part of rollback.
