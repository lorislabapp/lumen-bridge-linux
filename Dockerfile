# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.22-alpine AS build

WORKDIR /src

# Cache deps separately so source-only changes don't bust the module
# download layer. `go mod download` is faster than a full `go build`
# when only Go files (not go.mod/go.sum) changed.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary — no glibc, no surprises in the runtime image.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags='-s -w' \
    -o /out/lumen-bridge ./cmd/lumen-bridge

# ---- runtime stage ----
# distroless/static is ~2 MB, no shell, no package manager. The bridge
# doesn't need any of that — it's a pure Go process talking to MQTT
# (TCP) and HTTPS. CA roots are bundled by distroless.
FROM gcr.io/distroless/static-debian12:nonroot

# /data is the canonical mount point — config.yaml + token cache live here
# so the user can persist them across container recreates with one volume.
VOLUME ["/data"]
WORKDIR /data

COPY --from=build /out/lumen-bridge /usr/local/bin/lumen-bridge

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/lumen-bridge"]
CMD ["run", "--config", "/data/config.yaml"]
