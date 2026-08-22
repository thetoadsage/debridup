FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/debridup ./cmd/debridup

FROM alpine:3.21
RUN apk add --no-cache su-exec \
  && addgroup -S debridup \
  && adduser -S -G debridup -h /data debridup
COPY --from=build /out/debridup /usr/local/bin/debridup
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint
VOLUME ["/data"]
ENV DEBRIDUP_DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["/usr/local/bin/docker-entrypoint"]
