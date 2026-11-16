# Everything the screenshot runner needs, for hosts that have only Docker.
# Versions follow go.mod, CI's Node version, package.json and Playwright.
FROM golang:1.27.4-bookworm
# myhostname resolves *.localhost to loopback, as browsers and systemd hosts do.
RUN apt-get update && apt-get install -y --no-install-recommends \
    libnss-myhostname \
    poppler-utils \
    tmux \
    util-linux \
    xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && sed -i 's/^hosts:.*/& myhostname/' /etc/nsswitch.conf
RUN case "$(dpkg --print-architecture)" in arm64) ARCH=arm64 ;; *) ARCH=x64 ;; esac && \
    curl -fsSL "https://nodejs.org/dist/v26.13.0/node-v26.13.0-linux-${ARCH}.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 && \
    npm install -g npm@12.2.1
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN npx -y playwright@1.64.1 install --with-deps chromium && chmod -R a+rX /ms-playwright
