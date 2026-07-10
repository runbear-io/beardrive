# Build a static, CGO-free bdrive binary and ship it on distroless.
# The web server binds --addr (default :8080, which Cloud Run expects); GCS
# access uses Application Default Credentials (the runtime service account on
# Cloud Run / GCE — no key file needed).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /bdrive ./cmd/bdrive

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bdrive /bdrive
# Device identity + any file-backed state live here; mount a volume to persist
# it, or rely on a SQL database for metadata (recommended on Cloud Run).
ENV BDRIVE_HOME=/tmp/bdrive
ENTRYPOINT ["/bdrive"]
# Args are supplied at deploy time, e.g.:
#   web s3://... --addr :8080        (flags)
#   web -c /config/config.json       (mounted config)
