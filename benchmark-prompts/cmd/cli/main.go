// Command bench 是 benchmark 提示词平台的本地客户端。
//
// 它是 DSH / Pi 插件的唯一入口：插件不实现任何业务逻辑，只调用本命令的
// --json 输出（见 docs/plugins.md）。因此这里的退出码与 JSON 结构属于对外契约。
//
// 全部逻辑在 internal/cli，本文件只负责信号与退出码。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/benchmark-prompts/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.New(os.Stdout, os.Stderr).Run(ctx, os.Args[1:]))
}
