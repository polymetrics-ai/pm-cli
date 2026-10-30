# PM foundation CI job image (linux/amd64 only).
#
# This image is the native environment for CI. Its /usr comes from a
# digest-pinned Ubuntu base plus packages from one fixed snapshot.ubuntu.com
# timestamp, so host package upgrades cannot change it. build/native-environment.json
# is captured inside this image (build/ci/capture-native-profile.py) and
# tools/bootstrap.py verifies it before every native build.
#
# Go, Clang, Bazel, Compose, Node and Chromium are not installed here. Bootstrap
# downloads the locked, SHA-256-verified archives into the mounted cache.
# scripts/ci/in-ci-image.sh tags the image by the SHA-256 of this file and
# requirements.txt. Any change to either file builds a new image, and a change
# to the profiled trees needs a new reviewed native-environment.json.
ARG UBUNTU=docker.io/library/ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac
ARG SNAPSHOT=20261001T000000Z

FROM ${UBUNTU} AS apt-base
ARG SNAPSHOT
# apt checks the archive signature (ubuntu-archive-keyring) for every index, so
# TLS peer checks can stay off only for the first ca-certificates install.
RUN set -eu; \
    test "$(dpkg --print-architecture)" = amd64; \
    rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*; \
    printf '%s\n' 'Types: deb' \
      "URIs: https://snapshot.ubuntu.com/ubuntu/${SNAPSHOT}" \
      'Suites: noble noble-updates noble-security' \
      'Components: main universe' \
      'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' \
      > /etc/apt/sources.list.d/pmcli-snapshot.sources; \
    printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'APT::Install-Recommends "false";' \
      > /etc/apt/apt.conf.d/90pmcli; \
    apt-get -o Acquire::https::Verify-Peer=false update; \
    DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::https::Verify-Peer=false \
      install -y ca-certificates; \
    rm -rf /var/lib/apt/lists/*

FROM apt-base AS fetch
RUN set -eu; apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y curl xz-utils; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /fetch
RUN set -eu; \
    get() { curl -fsSL --proto '=https' --tlsv1.2 -o "$1" "$2"; \
            printf '%s  %s\n' "$3" "$1" | sha256sum -c -; }; \
    get docker.tgz https://download.docker.com/linux/static/stable/x86_64/docker-29.8.1.tgz \
      d8db66739d2e28d4933786d73e918d9be643a67fbd835db1bf740d650a259e70; \
    get actionlint.tgz https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz \
      8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8; \
    get shellcheck.txz https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.xz \
      8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198; \
    mkdir -p out; \
    tar -xzf docker.tgz docker/docker && install -m 0755 docker/docker out/docker; \
    tar -xzf actionlint.tgz actionlint && install -m 0755 actionlint out/actionlint; \
    tar -xJf shellcheck.txz shellcheck-v0.11.0/shellcheck && \
      install -m 0755 shellcheck-v0.11.0/shellcheck out/shellcheck

FROM apt-base
# Userland for the foundation: Python and Git for the checks, libc headers and
# GNU runtimes for cgo (the locked Clang/ld.lld comes from bootstrap), and the
# Chromium headless-shell runtime libraries for scripts/browser.sh.
RUN set -eu; apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
      git python3 python3-venv libc6-dev libgcc-s1 libstdc++6 \
      libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 \
      libcairo2 libcups2t64 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0t64 \
      libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 \
      libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/*.bin
COPY --from=fetch /fetch/out/ /opt/pmcli/bin/
COPY requirements.txt /opt/pmcli/requirements.txt
RUN set -eu; python3 -m venv /opt/pmcli/venv; \
    /opt/pmcli/venv/bin/python3 -m pip install --no-cache-dir --disable-pip-version-check \
      --no-deps --require-hashes -r /opt/pmcli/requirements.txt
ENV PATH=/opt/pmcli/venv/bin:/opt/pmcli/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LANG=C.UTF-8 \
    PYTHONDONTWRITEBYTECODE=1
