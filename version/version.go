// Package version 提供构建时注入的版本信息。
//
// Version 和 Commit 在编译时通过 -ldflags 注入:
//
//	go build -ldflags "-X github.com/volcano-tts/tts-api/version.Version=$VERSION \
//	                   -X github.com/volcano-tts/tts-api/version.Commit=$COMMIT"
//
// 开发时默认 "dev",CI/CD 时通常由 git describe 自动算出:
//   VERSION=$(git describe --tags --always --dirty)
//   COMMIT=$(git rev-parse --short HEAD)
//
// /health 端点会暴露这两个值,方便运维确认"跑的到底是哪个 commit"。
package version

var (
	Version = "dev"
	Commit  = "dev"
)
