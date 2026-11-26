# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# schema.sql, migrations/*.sql and seed_data.json are //go:embed'ed into the binary
RUN go build -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -ldflags="-s -w" -o /out/create-admin ./cmd/create-admin \
 && go build -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck \
 && mkdir -p /out/data/uploads

# ---- runtime ----
# distroless has no shell and no curl/wget, so the compose healthcheck runs the
# tiny /healthcheck binary (GET /healthz on localhost) instead of HEALTHCHECK here.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/create-admin /out/healthcheck /
# local-disk uploads live on a volume; the dir must be owned by the nonroot user (65532)
COPY --from=build --chown=65532:65532 /out/data /data
ENV PORT=8081 UPLOAD_DIR=/data/uploads
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/api"]
