# Build stage: dashboard + Go binary
FROM node:22-alpine AS dashboard
WORKDIR /app/web/dashboard
COPY web/dashboard/package*.json ./
RUN npm ci --production=false
COPY web/dashboard/ .
RUN npm run build

FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=dashboard /app/web/dashboard/dist ./web/dashboard/dist
RUN CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w" -o sage-router ./cmd/sage-router

# Runtime: minimal image with TLS certs and writable data dir
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    adduser -D -h /home/sage sage && \
    mkdir -p /home/sage/.sage-router && \
    chown sage:sage /home/sage/.sage-router
COPY --from=builder /app/sage-router /usr/local/bin/sage-router
USER sage
EXPOSE 20128
VOLUME /home/sage/.sage-router
ENTRYPOINT ["sage-router"]
