# Build stage. --platform=$BUILDPLATFORM: run the compiler on the native
# build host and CROSS-compile via TARGETOS/TARGETARCH below — Go needs no
# emulation to target another CPU. Without this, buildx emulates the whole
# toolchain under QEMU for non-native platforms (~45 min for arm64 on
# amd64 runners); with it, a multi-arch build is a few minutes. Only the
# final stage below is per-platform, and nothing executes there.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/tsidp-operator ./cmd

# Runtime: distroless static, non-root. The operator is pure Go (CGO off),
# so the static base suffices.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/tsidp-operator /tsidp-operator
USER 65532:65532
ENTRYPOINT ["/tsidp-operator"]
