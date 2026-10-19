FROM alpine:3.21
WORKDIR /app
RUN apk --no-cache add ca-certificates curl && \
    adduser -D -u 10001 steadfast && \
    mkdir -p /data && chown -R steadfast:steadfast /data /app
ARG TARGET=api
COPY bin/${TARGET} /app/service
COPY web /app/web
USER steadfast
ENTRYPOINT ["/app/service"]
