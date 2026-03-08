# Global ARG for use in FROM instructions
ARG OPENCLAW_VERSION=latest

# Build Go proxy
FROM golang:1.22-bookworm AS proxy-builder

WORKDIR /proxy
COPY proxy/ .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /proxy-bin .


# Extend pre-built OpenClaw with our auth proxy
FROM ghcr.io/openclaw/openclaw:${OPENCLAW_VERSION}

# Base image ends with USER node; switch to root for setup
USER root

# Add packages for openclaw agent operations + headless browser deps
# NOTE: Playwright can download Chromium without sudo at runtime, but the Chromium binary
# still needs system libs; install them at build time.
RUN apt-get update && apt-get install -y --no-install-recommends \
  ripgrep \
  ca-certificates \
  wget \
  gnupg \
  fonts-liberation \
  libasound2 \
  libatk-bridge2.0-0 \
  libatk1.0-0 \
  libcups2 \
  libdrm2 \
  libgbm1 \
  libgtk-3-0 \
  libnspr4 \
  libnss3 \
  libx11-xcb1 \
  libxcomposite1 \
  libxdamage1 \
  libxfixes3 \
  libxrandr2 \
  xdg-utils \
  && rm -rf /var/lib/apt/lists/*

# Install Playwright (kept local to this image) and pre-fetch Chromium + ffmpeg
# so the browser tool works immediately after deploy.
RUN npm i -g playwright@latest \
  && playwright install --with-deps chromium

# Add proxy
COPY --from=proxy-builder /proxy-bin /usr/local/bin/proxy

# Create CLI wrapper (openclaw code is at /app/dist/index.js in base image)
RUN printf '#!/bin/sh\nexec node /app/dist/index.js "$@"\n' > /usr/local/bin/openclaw \
  && chmod +x /usr/local/bin/openclaw

ENV PORT=10000
EXPOSE 10000

# Run as non-root for security (matching base image)
USER node

CMD ["/usr/local/bin/proxy"]
