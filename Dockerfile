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
        default-mysql-client \
        postgresql-client \
    && curl -fsSL https://www.mongodb.org/static/pgp/server-7.0.asc \
        | gpg --dearmor -o /usr/share/keyrings/mongodb-server-7.0.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/mongodb-server-7.0.gpg] https://repo.mongodb.org/apt/debian bookworm/mongodb-org/7.0 main" \
        > /etc/apt/sources.list.d/mongodb-org-7.0.list \
    && apt-get update && apt-get install -y --no-install-recommends mongodb-database-tools \
    && apt-get purge -y gnupg curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/mongodb-org-7.0.list /usr/share/keyrings/mongodb-server-7.0.gpg

COPY --from=builder /out/backupdb /usr/local/bin/backupdb

WORKDIR /app

ENTRYPOINT ["backupdb"]
CMD ["consumer"]
