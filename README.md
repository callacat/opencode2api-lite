# opencode2api-lite

把 **OpenCode Zen** 免费模型服务转成标准 API 的轻量代理：对外暴露 **OpenAI Chat Completions / OpenAI Responses / Anthropic Messages** 三种协议，免 API key、单文件 Go 实现、零第三方依赖，内置管理面板与 SOCKS5 出口调度。

```text
客户端（OpenAI / Anthropic SDK、Claude Code、各类面板）
        │
        ▼
opencode2api-lite  :8000
  ├─ /v1/chat/completions   OpenAI 协议
  ├─ /v1/responses          OpenAI Responses 协议
  ├─ /v1/messages           Anthropic 协议
  └─ /                      管理面板（模型别名 / 出口代理 / 用量统计）
        │   模型别名解析 + 客户端身份伪装 + 免费额度工具补齐
        ▼
https://opencode.ai/zen/v1/...     （可经 SOCKS5 出口）
```

> [!WARNING]
> 管理面板默认**不启用登录验证**（`-password` 留空）。对外暴露时务必设置密码，或用防火墙限制来源——面板可读写全部配置。
>
> 本项目为第三方代理实现，与 OpenCode 官方无关，仅供学习研究使用；请遵守上游服务条款，风险自担。

## 特性

- **三协议入口**：`/v1/chat/completions`（流式 / 非流式——非流式请求由代理以上游流式发出并本地聚合还原）、`/v1/responses`、`/v1/messages`（Anthropic，Claude Code 等可直连）
- **模型别名**：客户端模型名 → 上游 zen 模型名映射；未在别名表中配置的模型一律 403，防误用
- **多模态**：别名可配 `multimodal_model`，带图片的请求自动切换上游模型
- **SOCKS5 出口调度**：直连 / 固定代理 / 轮询 / 429 限速自动切换（含/不含直连两种模式）
- **身份自动刷新**：启动时从 npm 拉取 opencode 最新版本号填入 UA（低于上游门槛自动抬升），每日统计重置时同步刷新 session / project
- **管理面板**：模型别名、推理强度映射、出口代理在线编辑（保存即热加载）；用量统计按模型/按日；上游模型列表；会话手动刷新
- **单文件零依赖**：仅 Go 标准库，`go build` 直接出二进制，交叉编译全平台

## 快速开始

### 二进制

```bash
# 从 Releases 下载对应平台产物（校验见 sha256sums.txt）
./opencode2api-lite-1.0.0-linux-amd64 -port 8000 -password your-password
```

### Docker

```bash
docker run -d --name opencode2api-lite \
  -p 8000:8000 \
  -v "$PWD/data:/data" \
  ghcr.io/callacat/opencode2api-lite:latest \
  -password your-password
```

Compose 见 [`docker-compose.example.yml`](docker-compose.example.yml)；镜像支持 `linux/amd64` 与 `linux/arm64`。

### 验证

```bash
# 上游模型列表
curl -s http://127.0.0.1:8000/v1/models

# OpenAI 协议（模型名需先在 config.json / 面板中配置别名）
curl -s http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true}'

# Anthropic 协议（Claude Code：把 ANTHROPIC_BASE_URL 指到本服务即可）
curl -s http://127.0.0.1:8000/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}'
```

## 配置

首次启动会在工作目录生成 `config.json`（缺失字段自动补默认值并回写）：

```json
{
  "model_alias": {
    "big-pickle": { "target_model": "big-pickle" },
    "claude-sonnet-4-5": { "target_model": "big-pickle", "multimodal_model": "mimo-v2.5" }
  },
  "reasoning_effort_map": { "big-pickle": "medium" },
  "socks5_proxies": [
    { "addr": "127.0.0.1:7890", "name": "local" }
  ],
  "active_socks5": ""
}
```

| 字段 | 说明 |
| --- | --- |
| `model_alias` | 客户端模型名 → `target_model`（上游 zen 模型）。**未列入的一律 403**；`target_model` 留空时取别名同名；兼容旧写法 `"别名": "目标模型"` |
| `multimodal_model` | 可选，带图片的请求改用该上游模型 |
| `reasoning_effort_map` | 可选，按模型指定 reasoning effort |
| `socks5_proxies` | 出口代理列表，每项 `addr` / `username` / `password` / `name` |
| `active_socks5` | 出口选择：`""` 直连；`"127.0.0.1:7890"` 指定代理；`"__round_robin__"` 每请求轮询；`"__rate_limit_switch__"` 遇 429 轮换（含直连）；`"__rate_limit_switch_no_direct__"` 同上但只用代理 |
| `oc_session_id` | 免费层必需的**真实会话 ID**（`ses_` 开头 + 26 位）。上游 2026-10 起校验会话存在性，编造的 `ses_*` 一律 403；未配置时代理返回 `503 OCSessionMissing`（而不是静默 403）。也可用环境变量 `OC_SESSION_ID` 注入。**注意：会话有效并不等于能过**——请求体的 `tools` 还必须同时含 `bash` 与 `read`（缺任一同样 403，由代理自动补齐，勿在客户端精简掉）。两道门都失效时，上游只回一个不区分来源的 `403 FreeTierError` |
| `oc_session_refresh_cmd` | 可选。定期执行的命令，从其输出里解析新的 `ses_*` 自动续命（默认每 30 分钟，`OC_SESSION_REFRESH_INTERVAL` 可调）。上游无匿名会话注册端点，会话只能由 OpenCode 客户端产生，有效期未公开量化 |
| `key_pool` | 可选，多凭据轮转。免费层限流维度是**（模型 × 凭据）**，单凭据的日预算就是整条链路的日预算，配上池子才能把总额度按凭据数放大。`enabled` 关闭 / `keys` 为空时鉴权回落到 `Bearer public`，行为与不配这一项完全一致。`strategy`：`sticky`（默认，按 `x-session-id` 粘住一条，中途不换）或 `round_robin`（每请求推进游标摊开负载）。`max_retries` 是凭据额度耗尽（429）后额外换几条重试，默认 4。`keys` 每条可写 `{"id","key","weight","enabled"}` 或直接写字符串；缺 `id` 按数组位置补 `k1..kN`，重复凭据加载时去重。凭据属敏感信息，不要提交进仓库 |

## HTTP 接口

| 端点 | 方法 | 说明 |
| --- | --- | --- |
| `/v1/chat/completions` | POST | OpenAI 协议，流式 / 非流式 |
| `/v1/responses` | POST | OpenAI Responses 协议 |
| `/v1/messages` | POST | Anthropic Messages 协议 |
| `/v1/models` | GET | 上游模型列表 |
| `/health` | GET | 存活检查，返回 `OK` |
| `/` | GET | 管理面板（`-password` 非空时需登录） |
| `/login` `/logout` | GET / POST | 面板登录 / 登出 |
| `/api/config` | GET / POST | 读取 / 保存配置（保存即热加载） |
| `/api/stats` | GET / DELETE | 用量统计 / 清零 |
| `/api/models` | GET | 上游全量模型列表（面板下拉用） |
| `/api/reload` | POST | 刷新会话身份并重拉模型列表 |

## 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-port` | `8000` | 监听端口 |
| `-listen` | 空 | 监听地址。**留空 = 绑全网卡**（与历史行为一致，容器靠 `ports:` 映射进来就靠它）；写 `127.0.0.1` 只绑回环；写非回环地址**必须同时设 `-password`**，否则拒绝启动 |
| `-config` | `config.json` | 配置文件路径 |
| `-password` | 空 | 管理面板密码；留空则不启用登录验证 |
| `-debug` | `false` | 调试日志 |

监听地址也可用环境变量 `OC_LISTEN_ADDR` 或配置里的 `listen_addr` 指定，优先级
`-listen > OC_LISTEN_ADDR > config.listen_addr`；三者都空则沿用历史行为（`":"+port`）。

> Android app 走的是另一条路：它**显式传** `-listen 127.0.0.1`（局域网开关打开才传 `0.0.0.0`
> 并强制面板密码），所以手机上默认只对本机可见，而服务器部署的默认行为一字未变。
> 本地 app 的安全默认由调用方承担，不改库的默认值——已有部署（含现网容器）升级后不受影响。

## Android

手机上本地跑代理，界面直接复用自带管理面板（WebView），不需要服务器。

### 安装与启动

1. 从 [Releases](../../releases) 下载 `oc2api-android-<version>.apk` 安装
   （`arm64-v8a` 真机 / `x86_64` 模拟器都在同一个包里）。
2. 打开 app → 设端口（默认 8000）→ 点 **启动**。
3. 常驻通知出现即代理已就绪；下方 WebView 会加载面板，地址是
   `http://127.0.0.1:8000/`。面板能直接读写配置（会话、别名、出口、凭据池）。

### 给手机内其它 app 用

把目标 app 的 base_url 指到本机：

```
http://127.0.0.1:8000/v1/chat/completions   # OpenAI 协议
http://127.0.0.1:8000/v1/messages           # Anthropic 协议（Claude Code）
```

两点容易踩：

- 目标 app 自己也要允许到 `127.0.0.1` 的**明文 HTTP**（Android 9+ 默认禁明文）。
  本 app 已用 `network_security_config.xml` 按域放行，但那只管本 app。
- 本机回环地址只有同机 app 能访问，别的设备要走局域网模式。

### 局域网共享（默认关闭）

局域网模式打开后代理绑 `0.0.0.0`，同一 Wi-Fi 下的设备就能用
`http://<手机IP>:8000/v1/...`。

> ⚠️ **面板密码在局域网模式下是强制的**（app 与代理侧各拦一次）：面板能改上游会话与
> 凭据池，无密码地暴露到局域网等于把凭据送人。默认关闭时只绑 `127.0.0.1`，
> 外部设备一律连不上。

### 长时后台

系统进入 Doze 会掐掉应用的网络访问，代理就接不上上游。app 里有个
**忽略电池优化** 按钮，装机后点一次；否则「关屏 30 分钟后仍可用」只在插着电时成立。

### 构建（新 ABI / 自定义）

发布由 CI 完成：`.github/workflows/android-apk.yml` 先用 NDK
`CGO_ENABLED=1 GOOS=android` 交叉编译出 `liboc2api.so`，再 `gradle assembleRelease`
签名出包。本机不需要 Go / NDK / Gradle。

签名：CI 优先用仓库 secrets 里的 `ANDROID_KEYSTORE_B64` / `STORE_PASSWORD` /
`KEY_ALIAS` / `KEY_PASSWORD`；没有就临时自签一份，**签名身份每次运行都不同，
不同次构建的 APK 不能互相覆盖安装** —— 要长期分发请在 secrets 里配一份固定 keystore。
keystore 与 APK 一律不入库。

## 构建

```bash
go build -o opencode2api-lite .     # 本机构建
go vet ./... && go test ./...       # 检查 + 单测
```

发布由 GitHub Actions 完成：

- [`build-release.yml`](.github/workflows/build-release.yml)：推送 `v*` tag → 5 平台二进制 + `sha256sums.txt` → GitHub Release（也支持手动触发验证）
- [`docker-ghcr.yml`](.github/workflows/docker-ghcr.yml)：构建 `linux/amd64` + `linux/arm64` 镜像推送 GHCR（main → `latest`，tag → 语义化版本）
- [`android-apk.yml`](.github/workflows/android-apk.yml)：推送 `v*` tag → NDK 交叉编译 + Gradle 出签名 APK → artifact（tag 时附到 Release）

## License

[MIT](LICENSE)
