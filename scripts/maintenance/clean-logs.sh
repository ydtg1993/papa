#!/bin/bash
# 保留最近7天的日志
find ./logs -type f -name "*.log" -mtime +7 -delete
find ./downloads/.resume -type f -name "*.json" -mtime +1 -delete