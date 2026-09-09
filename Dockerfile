# 1-bosqich: Go ilovani Linux uchun yig'ish
FROM golang:1.22-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o app .

# 2-bosqich: Chromium brauzeri bilan ishlaydigan xavfsiz muhit
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
COPY .env .env

EXPOSE 10000

CMD ["./app"]