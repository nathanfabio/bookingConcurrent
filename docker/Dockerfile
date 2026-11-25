FROM golang:1.26.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate

FROM alpine:3.23 AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532

FROM runtime AS migrate
COPY --from=build /out/migrate /migrate
ENTRYPOINT ["/migrate"]
CMD ["up"]

FROM runtime AS worker
COPY --from=build /out/worker /worker
ENTRYPOINT ["/worker"]

FROM runtime AS api
COPY --from=build /out/api /api
ENTRYPOINT ["/api"]
