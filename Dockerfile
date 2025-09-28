# ---- Build stage ----
    FROM golang:1.25 AS builder

    WORKDIR /app
    COPY go.mod go.sum ./
    RUN go mod download
    
    COPY . .
    RUN CGO_ENABLED=0 GOOS=linux go build -o proxy main.go
    
    # ---- Runtime stage ----
    FROM alpine:3.20
    
    WORKDIR /app
    COPY --from=builder /app/proxy /app/proxy
    
    # Copy your proxies.json at runtime via volume mount
    EXPOSE 8080
    
    ENTRYPOINT ["/app/proxy", "proxies.json", ":8080"]
    