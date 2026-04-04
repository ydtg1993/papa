#!/bin/bash
# 启动 MySQL 容器并运行爬虫
docker-compose -f scripts/docker/docker-compose.yml up -d mysql
sleep 5
export APP_ENV=dev
go run cmd/crawler/main.go