# 1-bosqich: Go ilovani yig'ish (build)
FROM golang:1.22-bookworm AS builder
WORKDIR /app

# Barcha fayllarni birdaniga ko'chiramiz
COPY . .

# go mod download o'rniga to'g'ridan-to'g'ri build qilamiz
RUN CGO_ENABLED=0 GOOS=linux go build -o app .

# 2-bosqich: Chromium bilan ishlaydigan runtime muhit
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y \
    chromium \
    ca-certificates \
    fonts-liberation \
    fonts-noto-color-emoji \
    libasound2 \
    libnss3 \
    && rm -rf /var/lib/apt/lists/*

ENV CHROME_BIN=/usr/bin/chromium

WORKDIR /app
COPY --from=builder /app/app .

# Agar .env fayli bo'lsa ko'chiradi, bo'lmasa xato bermaydi
COPY .env* ./

EXPOSE 10000

CMD ["./app"]