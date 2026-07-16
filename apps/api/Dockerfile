# syntax=docker/dockerfile:1

# ── build ────────────────────────────────────────────────────────────
FROM golang:1.25 AS build
WORKDIR /src

# Cache modules separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
# Pure-Go static binary so it runs on a distroless/static base.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -ldflags "-s -w -X github.com/orpheus/api/internal/version.Version=${VERSION}" \
      -o /out/orpheus-api ./cmd/api

# ── runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/orpheus-api /app/orpheus-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/orpheus-api"]
