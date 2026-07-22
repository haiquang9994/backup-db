#syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backupdb ./cmd/backupdb

# ---- runtime stage ----------------------------------------------------------
# Debian, not Alpine: MongoDB only ships glibc builds of
# mongodb-database-tools (mongodump), which do not run on musl/Alpine.
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        tzdata \
        libpq5 \
    && curl -fsSL https://www.mongodb.org/static/pgp/server-7.0.asc \
        | gpg --dearmor -o /usr/share/keyrings/mongodb-server-7.0.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/mongodb-server-7.0.gpg] https://repo.mongodb.org/apt/debian bookworm/mongodb-org/7.0 main" \
        > /etc/apt/sources.list.d/mongodb-org-7.0.list \
    && apt-get update \
    # mariadb-client, postgresql-client-15, and mongodb-database-tools all
    # ship several helper binaries we never call (mariadb-admin/-import/...,
    # the perl wrapper scripts, mongorestore/mongoexport/mongostat/...), and
    # mariadb-client + postgresql-client-15 additionally drag in perl
    # (~46MB) just for their Debian multi-version wrapper mechanism. We only
    # ever exec mariadb-dump, pg_dump, and mongodump, and none of the three
    # actually link against perl, so download the .debs and extract just
    # those three files instead of installing the full packages.
    && cd /tmp && apt-get download mariadb-client postgresql-client-15 mongodb-database-tools \
    && dpkg -x mariadb-client_*.deb mariadb-extract \
    && dpkg -x postgresql-client-15_*.deb postgres-extract \
    && dpkg -x mongodb-database-tools_*.deb mongo-extract \
    && install -m 0755 mariadb-extract/usr/bin/mariadb-dump /usr/bin/mariadb-dump \
    && ln -s mariadb-dump /usr/bin/mysqldump \
    && install -m 0755 postgres-extract/usr/lib/postgresql/15/bin/pg_dump /usr/bin/pg_dump \
    && install -m 0755 mongo-extract/usr/bin/mongodump /usr/bin/mongodump \
    && rm -rf /tmp/mariadb-extract /tmp/postgres-extract /tmp/mongo-extract \
        /tmp/mariadb-client_*.deb /tmp/postgresql-client-15_*.deb /tmp/mongodb-database-tools_*.deb \
    && cd / \
    && apt-get purge -y gnupg curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/mongodb-org-7.0.list /usr/share/keyrings/mongodb-server-7.0.gpg

COPY --from=builder /out/backupdb /usr/local/bin/backupdb

WORKDIR /app

ENTRYPOINT ["backupdb"]
CMD ["consumer"]
