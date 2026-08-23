FROM vllm/vllm-openai@sha256:4f986370d7737abacc70ac17f86695acd1dc7892a02ad89ac132639d5afee0d0

ARG GLMOCR_SDK_REVISION=cef4d0ea120d1741f5cefe8985eee45f6c8eff1d

RUN python3 -m venv --system-site-packages /opt/glmocr-venv

ENV PATH="/opt/glmocr-venv/bin:${PATH}"

RUN python3 -m pip install --no-cache-dir --no-deps \
      "transformers==5.15.1" \
      "huggingface-hub==1.28.0" \
      "hf-xet==1.6.0" \
      "safetensors==0.8.0" \
      "portalocker==3.2.0" \
      "pymupdf==1.27.2.3" \
      "pypdfium2==5.6.0" \
      "flask==3.1.2" \
      "gunicorn==23.0.0" \
      "blinker==1.9.0" \
      "click==8.4.2" \
      "itsdangerous==2.2.0" \
      "werkzeug==3.1.8" \
    && python3 -m pip install --no-cache-dir --no-deps \
      "glmocr @ https://github.com/zai-org/GLM-OCR/archive/${GLMOCR_SDK_REVISION}.zip" \
    && python3 -c "from importlib.metadata import distribution; import flask, glmocr, pymupdf, torch, transformers; assert '${GLMOCR_SDK_REVISION}' in distribution('glmocr').read_text('direct_url.json'); assert pymupdf.pymupdf_version == '1.27.2.3'; assert pymupdf.mupdf_version == '1.27.2'; print(glmocr.__file__, pymupdf.pymupdf_version, pymupdf.mupdf_version, torch.__version__, transformers.__version__)"

COPY safe_server.py /opt/glmocr-venv/lib/python3.12/site-packages/safe_server.py

ENTRYPOINT []
