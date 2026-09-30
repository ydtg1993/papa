.PHONY: build cli test race vet clean

build:
	go build ./...

# 安装脚手架 CLI 到 $GOPATH/bin（go install 自动处理 Windows 的 .exe 后缀）
cli:
	go install ./cmd/papa

test:
	go test ./...

# 竞态检测：需要 C 编译器（Windows 上装 MinGW gcc 即可，本机已装）
race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin/
