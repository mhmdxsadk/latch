# syntax=docker/dockerfile:1
# Cross-compiles on the build host (no QEMU) for the requested target platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/latch ./cmd/latch

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/latch /usr/local/bin/latch
# NOTE: never mount the Docker socket into an AI agent's container.
USER nonroot
ENTRYPOINT ["/usr/local/bin/latch"]
CMD ["serve", "--config", "/etc/latch/config.yaml"]
