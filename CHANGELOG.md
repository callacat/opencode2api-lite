# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与[语义化版本](https://semver.org/lang/zh-CN/)。

## [v1.0.0] - 2026-09-22

### 新增

- 初始版本：OpenCode Zen → OpenAI Chat Completions / OpenAI Responses / Anthropic Messages 三协议代理
- 模型别名与多模态模型映射（未配置模型一律 403 拦截）
- SOCKS5 出口调度：直连 / 固定代理 / 轮询 / 429 限速自动切换
- 身份刷新：UA 版本从 npm 拉取并保证不低于上游门槛，每日统计重置时同步刷新 session
- 管理面板：配置在线编辑、用量统计（按模型/按日）、上游模型列表、会话手动刷新
- GitHub Actions：5 平台二进制 + `sha256sums.txt` 发 Release；GHCR 多架构镜像（linux/amd64, linux/arm64）
