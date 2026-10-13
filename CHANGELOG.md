# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与[语义化版本](https://semver.org/lang/zh-CN/)。

## [v1.0.1] - 2026-10-13

### 修复

- **免费层全量 403 FreeTierError**：上游 2026-10 把 `x-session-id` 的校验从「格式校验」升级为
  「存在性校验」。v1.0.0 依赖 `newOCSessionID()` 凭空编造一个格式合规的 `ses_*`（注释原文：
  「随机编造即可，无需服务端注册」），该假设已失效，编造值一律被拒。
  实测对照矩阵（2026-10-13）确认判定条件是**会话真实存在**，且是唯一变化的那一项：
  content 用 parts 数组还是字符串、read 参数 `file_path` 还是 `filePath`、有无 `stream_options`
  均无影响；携带真实 `oc_sk_*` API key、或补齐 `x-opencode-client/project/request/session/session-id`
  新协议头，也都无法让编造会话通过。
  另经复核：`tools` 门仍然生效（用真实会话复测——完全不带 tools → 403，去掉 `read` 的
  bash/glob/grep → 403，只带 `bash`+`read` → 200），`ensureFreeTierTools` 必须保留，不可删。

### 新增

- 配置项 `oc_session_id` 与环境变量 `OC_SESSION_ID`：注入 OpenCode 客户端真实创建过的会话。
  未注入时退回编造并在启动日志打印告警，直言该路径必然 403，不再静默失败。
- 管理面板新增「OpenCode 会话 ID」输入项，保存配置时随其余字段一起回写（此前面板「保存全部」
  只提交已知字段，新增字段若不回写会被静默清空）。
- `TestOCSessionInjection` / `TestNormalizeConfigTrimsSession` 两个自检，钉住会话选取与去空白。

### 已知限制

- 上游未提供匿名会话注册端点（`zen/v1/session(s)`、`inference/v1/session(s)`、`api/session`
  均 404；客户端二进制中也无注册端点），因此真实会话只能由 OpenCode 客户端产生后注入。
- 会话有效期未量化：实测 2026-10-13 16:13 产生的会话至 17:4x 仍返回 200，但过期时间未知。
  会话失效后表现为 403 FreeTierError，需重新注入。

### 回滚

回滚到 v1.0.0 即恢复旧行为（编造会话、必然 403）。新增字段均为 `omitempty`，
旧版本读取含 `oc_session_id` 的 config.json 会忽略该键，不影响启动。
面板新增输入项在旧版本不存在，忽略即可。

## [v1.0.0] - 2026-09-22

### 新增

- 初始版本：OpenCode Zen → OpenAI Chat Completions / OpenAI Responses / Anthropic Messages 三协议代理
- 模型别名与多模态模型映射（未配置模型一律 403 拦截）
- SOCKS5 出口调度：直连 / 固定代理 / 轮询 / 429 限速自动切换
- 身份刷新：UA 版本从 npm 拉取并保证不低于上游门槛，每日统计重置时同步刷新 session
- 管理面板：配置在线编辑、用量统计（按模型/按日）、上游模型列表、会话手动刷新
- GitHub Actions：5 平台二进制 + `sha256sums.txt` 发 Release；GHCR 多架构镜像（linux/amd64, linux/arm64）
