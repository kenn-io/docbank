FROM ubuntu:24.04@sha256:24737cef17f4e22b1977df7d9a05e77c4e28b7da5b8911f8d8ccda136bf3573c

LABEL org.opencontainers.image.source="https://github.com/kenn-io/docbank"

ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'path-include=/usr/share/doc/libreoffice-common/examples/*' > /etc/dpkg/dpkg.cfg.d/zz-libreoffice-examples \
    && apt-get \
      -o Acquire::Retries=5 \
      -o Acquire::http::Timeout=30 \
      -o Acquire::https::Timeout=30 \
      update \
    && apt-get \
      -o Acquire::Retries=5 \
      -o Acquire::http::Timeout=30 \
      -o Acquire::https::Timeout=30 \
      install -y --no-install-recommends \
      libreoffice-writer libreoffice-impress libreoffice-calc poppler-utils \
      ca-certificates python3 procps git fonts-dejavu-core tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home proof
