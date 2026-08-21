FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/debridup ./cmd/debridup

FROM alpine:3.21
RUN addgroup -S debridup && adduser -S -G debridup -h /data debridup
COPY --from=build /out/debridup /usr/local/bin/debridup
USER debridup
VOLUME ["/data"]
ENV DEBRIDUP_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/debridup"]
