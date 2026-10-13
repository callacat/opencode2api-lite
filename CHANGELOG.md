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
  **未配置时不再编造**（编造必然 403），直接返回 `503 OCSessionMissing` 并在错误信息里写明
  要配什么——静默的 403 对调用方毫无信息量。
- 配置项 `oc_session_refresh_cmd` 与环境变量 `OC_SESSION_REFRESH_INTERVAL`（默认 30m）：
  代理定期执行该命令，从输出里解析 `ses_[0-9a-f]{12}[A-Za-z0-9]{14}` 并写回内存与 config.json，
  解决会话过期后的续命问题（实测有效期下限 52 分钟，故默认间隔取其一半留一倍余量）。
  典型命令：`opencode run "x" 2>&1 | grep -o 'ses_[0-9a-f]\{12\}[A-Za-z0-9]\{14\}'`
- 管理面板新增「OpenCode 会话 ID」与「会话自动续命命令」两项输入，保存时一并回写。
- 面板回传配置时若请求体省略 `oc_session_id` 会沿用现值——旧版前端/脚本只提交已知字段，
  直接解进新 struct 会把会话静默清空、服务随即退回 403。
- 自检扩充到 9 个用例：`TestOCSessionInjection`（含「未配置不编造」）、
  `TestOCSessionMissingBody`、`TestOCSessionRe`、`TestRefreshOCSessionOnce`、
  `TestNormalizeConfigTrimsSession`、`TestMergeOCSessionID`、`TestConfigPostDecodePresence`。

### 已知限制

- 上游未提供匿名会话注册端点（`zen/v1/session(s)`、`inference/v1/session(s)`、`api/session`
  均 404；客户端二进制中也无注册端点），因此真实会话只能由 OpenCode 客户端产生后注入。
- 会话有效期未量化：实测 2026-10-13 16:13 产生的会话至 17:2x 仍返回 200（下限 ≥52 分钟），
  上限未知。`oc_session_refresh_cmd` 的默认 30 分钟间隔建立在这个下限之上；若实际 TTL
  短于该间隔，需调小 `OC_SESSION_REFRESH_INTERVAL`。
- 会话刷新命令需要能创建真实会话的凭据（如本机 `opencode run` + `auth.json` 里的 oc_sk key），
  该凭据不随本项目分发，部署方需自备。

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
