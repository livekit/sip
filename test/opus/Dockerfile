# Stage 1: Build livekit-sip with Opus changes
FROM golang:1.24-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus-dev libopusfile-dev pkg-config && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache dependency download
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the SIP service binary
RUN CGO_ENABLED=1 go build -o /livekit-sip ./cmd/livekit-sip/

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus0 ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /livekit-sip /usr/local/bin/livekit-sip

ENTRYPOINT ["/usr/local/bin/livekit-sip"]