# 多阶段构建
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o papa cmd/crawler/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates chromium ffmpeg
WORKDIR /root/
COPY --from=builder /app/papa .
COPY configs/config.yaml ./configs/
COPY storage/architecture.png ./storage/
EXPOSE 9090
CMD ["./papa"]