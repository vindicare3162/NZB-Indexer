# syntax=docker/dockerfile:1

# --- Stage 1: build the Svelte SPA ---
FROM node:22-alpine AS web
WORKDIR /web
# Install dependencies first for better layer caching.
COPY web/package.json web/package-lock.json ./
RUN npm ci
# Build the frontend into /web/dist.
COPY web/ ./
RUN npm run build

# --- Stage 2: build the Go binary (with the SPA embedded) ---
FROM golang:1.27-alpine AS build
WORKDIR /src
# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download
# Copy the source, then overlay the freshly built SPA so go:embed picks it up.
COPY . .
COPY --from=web /web/dist ./web/dist
# Static binary, version stamped from the build arg.
ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/goindex ./cmd/goindex

# --- Stage 3: minimal runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S goindex && adduser -S -G goindex goindex
COPY --from=build /out/goindex /usr/local/bin/goindex
USER goindex

ENV GOINDEX_SERVER_LISTEN_ADDR=":8080"
EXPOSE 8080

# The binary probes its own health endpoint, so no curl/wget is needed.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/goindex", "healthcheck", "-addr", "http://127.0.0.1:8080"]

ENTRYPOINT ["/usr/local/bin/goindex"]
CMD ["serve"]
