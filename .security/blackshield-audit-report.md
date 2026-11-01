# BlackShield Security Audit Report — `kenn-io/docbank`

- **Scanned Commit:** `85ea4f738a59d4c73f5b02f5f621a99584e5e4c4`
- **Actionable Findings:** `3` (`0 Critical`, `3 High`, `68 Medium`, `3 Low`)
- **BlackShield Platform & Continuous Zero-Cost PR Protection:** [https://blackshield.chaplau.com](https://blackshield.chaplau.com)

## Detailed Findings & Remediation Guide

### 1. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `.github/docker/render-pdf/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 2. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `deploy/glmocr/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 3. [HIGH] `sec-broken-crypto-cipher` — sec-broken-crypto-cipher: Deprecated or Weak Symmetric Cipher Algorithm

- **Category:** `sast`
- **Location:** `document/media/mediatest/mediatest.go:390`
- **Details:** Using broken cryptographic algorithms (DES, 3DES, RC4, Blowfish, AES-ECB mode) compromises data confidentiality. at document/media/mediatest/mediatest.go:390
- **Recommended Fix:** Use AES-256-GCM or ChaCha20-Poly1305 for symmetric encryption. Avoid ECB mode.

