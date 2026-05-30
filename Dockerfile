# --- Build stage ---
FROM golang:1.26-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

RUN apk add --no-cache git

WORKDIR /src

# Cache dependencies
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

# Copy server source
COPY server/ ./server/

# Build binaries in parallel
ARG VERSION=dev
ARG COMMIT=unknown
RUN cd server && ( \
    CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o bin/server ./cmd/server & p1=$!; \
    CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o bin/multica ./cmd/multica & p2=$!; \
    CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/migrate ./cmd/migrate & p3=$!; \
    CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/backfill_task_usage_hourly ./cmd/backfill_task_usage_hourly & p4=$!; \
    wait "$p1"; s1=$?; wait "$p2"; s2=$?; wait "$p3"; s3=$?; wait "$p4"; s4=$?; \
    test "$s1" -eq 0 -a "$s2" -eq 0 -a "$s3" -eq 0 -a "$s4" -eq 0 \
)

# --- Runtime stage ---
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /src/server/bin/server .
COPY --from=builder /src/server/bin/multica .
COPY --from=builder /src/server/bin/migrate .
COPY --from=builder /src/server/bin/backfill_task_usage_hourly .
COPY server/migrations/ ./migrations/
COPY docker/entrypoint.sh .
RUN sed -i 's/\r$//' entrypoint.sh && chmod +x entrypoint.sh

EXPOSE 8080

ENTRYPOINT ["./entrypoint.sh"]
