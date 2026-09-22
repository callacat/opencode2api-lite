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
| `-config` | `config.json` | 配置文件路径 |
| `-password` | 空 | 管理面板密码；留空则不启用登录验证 |
| `-debug` | `false` | 调试日志 |

## 构建

```bash
go build -o opencode2api-lite .     # 本机构建
go vet ./... && go test ./...       # 检查 + 单测
```

发布由 GitHub Actions 完成：

- [`build-release.yml`](.github/workflows/build-release.yml)：推送 `v*` tag → 5 平台二进制 + `sha256sums.txt` → GitHub Release（也支持手动触发验证）
- [`docker-ghcr.yml`](.github/workflows/docker-ghcr.yml)：构建 `linux/amd64` + `linux/arm64` 镜像推送 GHCR（main → `latest`，tag → 语义化版本）

## License

[MIT](LICENSE)
