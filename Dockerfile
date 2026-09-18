FROM golang:1.27-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org

ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

WORKDIR /src

COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal

RUN go mod edit \
    -dropreplace=github.com/maximhq/bifrost/core \
    -dropreplace=github.com/maximhq/bifrost/framework \
    -dropreplace=github.com/maximhq/bifrost/plugins/governance \
    -dropreplace=github.com/maximhq/bifrost/transports
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/maas-api ./cmd/maas-api

FROM alpine:3.22
RUN addgroup -S maas && adduser -S -G maas maas
COPY --from=build /out/maas-api /maas-api

# Keep dependency licensing material with the distributed image.
COPY LICENSE /licenses/maas/LICENSE
COPY licenses/bifrost/LICENSE /licenses/bifrost/LICENSE
COPY licenses/bifrost/THIRD_PARTY_NOTICES.md /licenses/bifrost/THIRD_PARTY_NOTICES.md

EXPOSE 8080
USER maas
ENTRYPOINT ["/maas-api"]
