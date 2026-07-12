FROM golang:1.25-alpine AS build
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=1 go build -o /out/filetrans ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
  && addgroup -S filetrans \
  && adduser -S -G filetrans filetrans \
  && mkdir -p /app/config /app/data /app/uploads /app/public \
  && chown -R filetrans:filetrans /app
WORKDIR /app
COPY --from=build --chown=filetrans:filetrans /out/filetrans /usr/local/bin/filetrans
EXPOSE 17878
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:17878/api/health/ready || exit 1
USER filetrans
CMD ["filetrans", "-config", "/app/config/config.yaml"]
