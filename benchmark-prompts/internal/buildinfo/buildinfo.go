// Package buildinfo 暴露构建期注入的版本信息。
//
// 值由 `go build -ldflags "-X .../internal/buildinfo.Version=v1.2.3"` 写入，
// 因此默认值 "dev" 只出现在源码直跑（go run / go test）时。
//
// 为什么不各自放在 cmd/*：bench CLI 与 bench-server 是两个二进制，
// 但部署时需要用同一个版本串回答"线上跑的是哪一版"，所以注入点必须唯一。
package buildinfo

// Version 是人读版本号，通常来自 git describe，回落到 VERSION 文件内容。
var Version = "dev"

// Commit 是构建时的短提交号；无 git 时为 "none"。
var Commit = "none"

// Date 是 UTC 构建时间（RFC3339）。留空表示未注入。
var Date = ""

// Map 返回适合 JSON 输出的字段集。
//
// 注意：向该 map 增加键属于**契约追加**（允许），删除或改名则违反 api.md 的
// "只增不改不删" 约定 —— bench version --json 是插件与运维脚本的解析对象。
func Map() map[string]any {
	m := map[string]any{"version": Version, "commit": Commit}
	if Date != "" {
		m["date"] = Date
	}
	return m
}

// String 返回一行人类可读版本，用于启动日志与 -version 输出。
func String() string {
	s := Version + " (commit " + Commit
	if Date != "" {
		s += ", built " + Date
	}
	return s + ")"
}
