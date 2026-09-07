ARG BASE_IMAGE=kyber/runtime-base:local

FROM debian:13.4 AS sqlite-builder
ARG SQLITE_AUTOCONF_VERSION=3530400
ARG SQLITE_SHA256=0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends build-essential ca-certificates curl && \
    (curl -fsSL --retry 1 --retry-all-errors --connect-timeout 15 --max-time 60 \
        -o /tmp/sqlite.tar.gz "https://sqlite.org/2026/sqlite-autoconf-${SQLITE_AUTOCONF_VERSION}.tar.gz" || \
     curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 --max-time 120 \
        -o /tmp/sqlite.tar.gz "https://sources.buildroot.net/sqlite/sqlite-autoconf-${SQLITE_AUTOCONF_VERSION}.tar.gz") && \
    printf '%s  %s\n' "${SQLITE_SHA256}" /tmp/sqlite.tar.gz | sha256sum -c - && \
    tar -xzf /tmp/sqlite.tar.gz -C /tmp && \
    cd "/tmp/sqlite-autoconf-${SQLITE_AUTOCONF_VERSION}" && \
    CFLAGS="-O2 -DSQLITE_ENABLE_FTS3 -DSQLITE_ENABLE_FTS3_PARENTHESIS -DSQLITE_ENABLE_FTS4 -DSQLITE_ENABLE_FTS5 -DSQLITE_ENABLE_RTREE -DSQLITE_ENABLE_GEOPOLY -DSQLITE_ENABLE_COLUMN_METADATA -DSQLITE_ENABLE_UNLOCK_NOTIFY -DSQLITE_ENABLE_DBSTAT_VTAB -DSQLITE_ENABLE_DBPAGE_VTAB -DSQLITE_ENABLE_MATH_FUNCTIONS -DSQLITE_ENABLE_PREUPDATE_HOOK -DSQLITE_ENABLE_SESSION -DSQLITE_SECURE_DELETE -DSQLITE_THREADSAFE=1 -DSQLITE_MAX_VARIABLE_NUMBER=250000" \
        ./configure --prefix=/opt/sqlite-fixed --disable-static && \
    make -j"$(nproc)" && make install

FROM golang:1.26-alpine AS skills-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/kyber-skills ./cmd/kyber-skills

FROM ghcr.io/astral-sh/uv:0.11.6-python3.13-trixie@sha256:b3c543b6c4f23a5f2df22866bd7857e5d304b67a564f4feab6ac22044dde719b AS uv-source

FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive
ENV PYTHONUNBUFFERED=1
ENV PYTHONDONTWRITEBYTECODE=1
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        build-essential libffi-dev libatomic1 openssh-client procps python3-yaml ripgrep xz-utils && \
    rm -rf /var/lib/apt/lists/*

# Hermes 0.21.0 carries session state in SQLite WAL mode. Preserve its upstream
# fixed SQLite floor rather than depending on the runtime-base distro version.
COPY --from=sqlite-builder /opt/sqlite-fixed/lib/libsqlite3.so.3.53.4 /usr/local/lib/
RUN ln -sf libsqlite3.so.3.53.4 /usr/local/lib/libsqlite3.so.0 && \
    ln -sf libsqlite3.so.3.53.4 /usr/local/lib/libsqlite3.so && \
    printf '/usr/local/lib\n' > /etc/ld.so.conf.d/000-sqlite-fixed.conf && ldconfig

COPY --chmod=0755 --from=uv-source /usr/local/bin/uv /usr/local/bin/uvx /usr/local/bin/

ARG HERMES_VERSION=0.21.0
ARG HERMES_COMMIT=29112bef099274229cadff79cdff7bf7b99c4b77
ENV UV_PYTHON_INSTALL_DIR=/opt/uv/python
RUN git init /opt/hermes && \
    git -C /opt/hermes remote add origin https://github.com/NousResearch/hermes-agent.git && \
    git -C /opt/hermes fetch --depth 1 origin "${HERMES_COMMIT}" && \
    test "$(git -C /opt/hermes rev-parse FETCH_HEAD)" = "${HERMES_COMMIT}" && \
    git -C /opt/hermes checkout --detach FETCH_HEAD && \
    cd /opt/hermes && \
    uv python install 3.13 && \
    uv sync --frozen --python 3.13 --extra all && \
    test "$(.venv/bin/python -c 'from importlib.metadata import version; print(version("hermes-agent"))')" = "${HERMES_VERSION}" && \
    .venv/bin/python -c 'import sqlite3, sys; sys.exit(0 if sqlite3.sqlite_version_info >= (3, 51, 3) else 1)' && \
    ln -s /opt/hermes/.venv/bin/hermes /usr/local/bin/hermes && \
    rm -rf /opt/hermes/.git && \
    chmod -R a+rX,go-w /opt/hermes /opt/uv

ENV KYBER_RUNTIME_DEFAULT_VERSION=${HERMES_VERSION}
ENV HERMES_HOME=/home/kyber/.hermes
ENV HERMES_DISABLE_LAZY_INSTALLS=1
ENV KYBER_START_CMD=/usr/local/bin/start-hermes.sh

COPY --chmod=0755 images/hermes/start-hermes.sh /usr/local/bin/start-hermes.sh
COPY --chmod=0755 images/hermes/configure-hermes.py /usr/local/bin/kyber-configure-hermes
COPY --chmod=0755 images/shared/kyber-identity-repo.sh /usr/local/bin/kyber-identity-repo.sh
COPY --from=skills-builder /build/kyber-skills /usr/local/bin/kyber-skills
RUN install -d -m 0755 /opt/kyber
COPY --chmod=0644 docs/agent-manual.md /opt/kyber/KYBER.md
COPY images/shared/skills/telegram-messaging /opt/kyber/skills/telegram-messaging
COPY images/shared/skills/discord-messaging /opt/kyber/skills/discord-messaging
COPY images/shared/skills/a2a-client /opt/kyber/skills/a2a-client
