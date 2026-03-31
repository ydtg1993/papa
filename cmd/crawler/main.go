package main

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/internal/app"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// 1. 创建应用容器
	appInstance, err := app.NewApp("configs/config.yaml")
	if err != nil {
		fmt.Printf("init app failed: %v\n", err)
		os.Exit(1)
	}
	// 2. 创建可取消的 context，用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3. 捕获退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel() // 收到信号时取消 context
	}()

	// 4. 运行应用（阻塞直到 ctx，内部自动清理资源）
	appInstance.Run(ctx)
}
