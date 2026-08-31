# syntax=docker/dockerfile:1

# --- build -------------------------------------------------------------------
FROM golang:1.27-alpine AS build

ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""

WORKDIR /src

# Dependencies first, so the module cache survives source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/restorelab/restorelab/internal/version.Version=${VERSION} \
        -X github.com/restorelab/restorelab/internal/version.Commit=${COMMIT} \
        -X github.com/restorelab/restorelab/internal/version.Date=${DATE}" \
      -o /out/restorelab ./cmd/restorelab

# --- runtime -----------------------------------------------------------------
FROM alpine:3.21

# ca-certificates: talking to Proxmox/PBS over TLS.
# tzdata: report timestamps in the operator's timezone.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /home/restorelab restorelab

COPY --from=build /out/restorelab /usr/local/bin/restorelab

USER restorelab
WORKDIR /home/restorelab

# Config and the master key are mounted in; the master key should come from
# RESTORELAB_MASTER_KEY as a container secret rather than a file.
ENV RESTORELAB_CONFIG=/home/restorelab/.restorelab/config.yaml
VOLUME ["/home/restorelab/.restorelab"]

ENTRYPOINT ["restorelab"]
CMD ["--help"]
