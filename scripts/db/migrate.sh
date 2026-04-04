#!/bin/bash
# 用于非 dev 环境手动迁移
export APP_ENV=dev
go run cmd/crawler/main.go