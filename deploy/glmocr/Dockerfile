FROM vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0

RUN python3 -m venv --system-site-packages /opt/glmocr-venv

ENV PATH="/opt/glmocr-venv/bin:${PATH}"

COPY requirements.lock /etc/glmocr/requirements.lock

RUN python3 -m pip install --no-cache-dir --no-build-isolation --no-deps --require-hashes \
      -r /etc/glmocr/requirements.lock \
    && python3 -c "from importlib.metadata import distribution; import flask, glmocr, pymupdf, torch, transformers; assert 'cef4d0ea120d1741f5cefe8985eee45f6c8eff1d' in distribution('glmocr').read_text('direct_url.json'); assert pymupdf.pymupdf_version == '1.27.2.3'; assert pymupdf.mupdf_version == '1.27.2'; print(glmocr.__file__, pymupdf.pymupdf_version, pymupdf.mupdf_version, torch.__version__, transformers.__version__)"

COPY Dockerfile /etc/glmocr/Dockerfile
COPY deployment.json /etc/glmocr/deployment.json
COPY engine_identity.py /opt/glmocr-venv/lib/python3.12/site-packages/engine_identity.py
COPY safe_server.py /opt/glmocr-venv/lib/python3.12/site-packages/safe_server.py

ENTRYPOINT []
