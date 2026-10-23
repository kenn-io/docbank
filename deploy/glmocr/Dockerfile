# syntax=docker/dockerfile:1
FROM vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates=20260816ubuntu0.24.04.1 \
    && rm -rf /var/lib/apt/lists/*

ADD --checksum=sha256:fd7f4dfa8190e014013b9f08f3e973f4e4c26ffd179a204895533a7254813a70 \
    https://files.pythonhosted.org/packages/3f/06/3e4420302363fbb71be3c7f35ce042f54bfca330e3e66b3a2e3c4cca579f/pip-26.2.2-py3-none-any.whl \
    /tmp/pip-26.2.2-py3-none-any.whl

RUN python3 -m venv --without-pip --system-site-packages /opt/glmocr-venv

ENV PATH="/opt/glmocr-venv/bin:${PATH}"

RUN python3 /tmp/pip-26.2.2-py3-none-any.whl/pip install --no-cache-dir --no-deps --ignore-installed \
      --no-index /tmp/pip-26.2.2-py3-none-any.whl \
    && rm /tmp/pip-26.2.2-py3-none-any.whl

COPY requirements.lock /etc/glmocr/requirements.lock

RUN python3 -m pip install --no-cache-dir --no-build-isolation --no-deps --require-hashes \
      -r /etc/glmocr/requirements.lock \
    && python3 -c "from importlib.metadata import distribution; import flask, glmocr, pymupdf, torch, transformers; assert 'cef4d0ea120d1741f5cefe8985eee45f6c8eff1d' in distribution('glmocr').read_text('direct_url.json'); assert pymupdf.pymupdf_version == '1.27.2.3'; assert pymupdf.mupdf_version == '1.27.2'; print(glmocr.__file__, pymupdf.pymupdf_version, pymupdf.mupdf_version, torch.__version__, transformers.__version__)"

COPY Dockerfile /etc/glmocr/Dockerfile
COPY deployment.json /etc/glmocr/deployment.json
COPY engine_identity.py /opt/glmocr-venv/lib/python3.12/site-packages/engine_identity.py
COPY safe_server.py /opt/glmocr-venv/lib/python3.12/site-packages/safe_server.py

ENTRYPOINT []
