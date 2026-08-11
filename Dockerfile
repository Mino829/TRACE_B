FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /trace-backend ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates wget && addgroup -S trace && adduser -S -G trace trace
WORKDIR /app
RUN mkdir /app/data && chown trace:trace /app/data
COPY --from=build /trace-backend /usr/local/bin/trace-backend
USER trace
ENV TRACE_ADDRESS=:8080 TRACE_DATABASE_PATH=/app/data/trace.db
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/health/ready || exit 1
ENTRYPOINT ["trace-backend"]
