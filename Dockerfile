FROM golang:1.27 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

# Distroless non-root final image — no shell, no package manager.
# Keeps the attack surface minimal and image size small (~10 MB).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /gateway /gateway
EXPOSE 18100
ENTRYPOINT ["/gateway"]
