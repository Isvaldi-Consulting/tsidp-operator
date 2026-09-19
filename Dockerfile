# Build stage
FROM golang:1.26 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tsidp-operator /tsidp-operator
USER 65532:65532
ENTRYPOINT ["/tsidp-operator"]
