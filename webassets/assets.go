package webassets

import "embed"

// Dist 保存前端构建产物，由 Go 编译阶段嵌入管理器二进制。
//
//go:embed dist
var Dist embed.FS
