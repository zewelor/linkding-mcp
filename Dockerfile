FROM golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go cli.go schema.go ./
COPY docs/cli-core.md ./docs/cli-core.md
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/linkding .

FROM scratch

LABEL org.opencontainers.image.source="https://github.com/zewelor/linkding"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/linkding /usr/local/bin/linkding

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/linkding"]
CMD ["mcp"]
