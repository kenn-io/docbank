FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db

LABEL org.opencontainers.image.source="https://github.com/kenn-io/docbank"

ENV DEBIAN_FRONTEND=noninteractive

# The path-include keeps the targets of LibreOffice's /etc/libreoffice/registry/*.xcd.sample symlinks because renderpdf runtime discovery follows them.
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
