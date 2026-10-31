# One image holds every Cuboid program; docker-compose.yml picks the command.

FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS go
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 cuboid
WORKDIR /app
COPY --from=go /out/ /usr/local/bin/
COPY --from=web /web/dist/ /app/web/dist/
COPY config/ /app/config/
COPY migrations/ /app/migrations/
RUN mkdir -p /app/artifacts && chown cuboid /app/artifacts
USER cuboid
ENV CUBOID_WEB_DIR=/app/web/dist CUBOID_METRICS=/app/config/metrics.yaml CUBOID_LIMITS=/app/config/limits.yaml CUBOID_ARTIFACTS=/app/artifacts
EXPOSE 8080
CMD ["supervisor", "--", "cuboid", "serve"]
