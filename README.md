# xai-autoban

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 原生插件：在 xAI 凭据返回 `401/402/403/429` 时自动隔离，避免 CPA 在大号池里逐个重试坏号、拖长首 token。

本仓库为 [vrxiaojie/xai-autoban](https://github.com/vrxiaojie/xai-autoban) 的 fork（amiibot），当前版本 **1.4.3**。

| | |
| --- | --- |
| 插件 id / 文件名 | `xai-autoban` |
| 商店 registry | https://raw.githubusercontent.com/amiibot/xai-autoban/main/registry.json |
| Latest Release | https://github.com/amiibot/xai-autoban/releases/latest |

---

## 功能概览

| 能力 | 说明 |
| --- | --- |
| 调度隔离 | Usage 失败后立即从调度候选中跳过该凭据 |
| Management 停用 | 后台 `PATCH` CPA 将 auth 标为 disabled；到期再启用 |
| 失败类型（展示 + TTL） | 解析 body 粗分为 `auth` / `permission` / `quota_free` 等；可按类型配隔离时长 |
| 永久删除 | **与原版一致：仅 HTTP 403**（删凭据文件，不是解禁） |
| 用量观测 | 内嵌只读 CPAMP `usage.sqlite` 近 24h token；**不预 ban** |
| 面板 | 统计、圆饼图、搜索筛选、解禁 / 删 403 |

只处理 `provider=xai`，不影响 Codex / Claude / Gemini 等。

---

## 隔离与失败类型

默认触发状态码：`401`、`402`、`403`、`429`。默认隔离 **24 小时**（`disable-hours` / `class-disable-hours` 可改）。

| 失败类型 | 典型信号 | 默认隔离 |
| --- | --- | --- |
| `auth` | 401；403 body 含 invalid/expired token | 24h |
| `payment` | 402 | 24h |
| `quota_paid` | spending-limit 等 | 24h |
| `quota_free` | free-usage-exhausted 等 | 24h |
| `permission` | permission-denied / chat endpoint denied | 24h |
| `rate_limit` | 429 | 24h |
| `forbidden_unknown` | 其它 403 | 24h |

失败类型用于**面板展示**和**按类调整 TTL**。  
**永久删除不看失败类型，只看状态码是否为 403**（上游原版行为）。

```yaml
plugins:
  configs:
    xai-autoban:
      enabled: true
      disable-hours: 24
      classify-body: true
      # class-disable-hours:
      #   rate_limit: 12
      #   permission: 48
```

---

## 永久删除（403）

| 操作 | 行为 |
| --- | --- |
| 行内「删除账号」 | 仅该行 `status_code === 403` |
| 「删除已选 403」 | 勾选中的 403 |
| 「删除全部 403 账号」 | 当前隔离列表里全部 403 |

调用 Management API **删除凭据文件**，不会重新启用。勾选主要用于「解禁已选」；删除也可对已选 403 批量执行。

---

## 用量观测与圆饼图（v1.3+）

- **单插件**：直接读 CPAMP / Usage 的 `usage.sqlite`，不依赖并排安装 grok-quota。
- **不预 ban**：用量高或额度日志**不会**触发隔离；隔离只看实时 401/402/403/429。
- 隔离行「24h用量 / 上限」显示如 `1.20M / 2.00M`（无 ok/cooldown 文案）。
- **池规模**：优先 Management 列出的 xAI 凭证总数；Management 失败时退化为 usage 中出现过的账号数。
- 路径：`usage-db-path` / 环境变量 `XAI_AUTOBAN_USAGE_DB`、`CPAMP_USAGE_DB`，或自动探测 `data/usage.sqlite` 等。
- 可选回退：`join-quota-state` + 外部 `grok-quota-state.json`。

---

## 面板列说明

| 列 | 含义 |
| --- | --- |
| 状态码 | HTTP 401/402/403/429 |
| 失败类型 | body 粗分类（仅展示 / TTL） |
| 24h用量 / 上限 | 观测用 token，不决策 |
| 原因 | 简短 reason |
| 当前处置 | 本地/CPA 进度：隔离中·待停用 / 已停用 / 解禁中 / 半开试用 n/2 / 停用失败·重试 / 解禁失败·重试 |
| 最后使用 | usage.sqlite 最近一次**成功**请求时间（跨 24h 窗口；只读） |
| 已下线(h) | 本轮连续不可用起点至今的整小时数；阶梯再隔离 / 试用失败**不重置**；毕业或手动解禁清空 |
| 剩余时间 | 本轮冷却剩余时长（hover 可看计划解禁时间；表中不再单独展示隔离开始/自动解禁） |

菜单：管理中心 → **xAI Autoban**  
资源：`/v0/resource/plugins/xai-autoban/status`、`/data`

---

## 安装

### 商店（推荐）

**请使用本 fork 的 registry**（上游仍为 1.0.4）：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/amiibot/xai-autoban/main/registry.json"
  configs:
    xai-autoban:
      enabled: true
      priority: 200
      # 必须与 CPA 实际监听端口一致（默认文档常写 8317，自建可能是 19999）
      management-url: http://127.0.0.1:19999
      management-key-env: CPA_MANAGEMENT_KEY
      disable-hours: 24
      status-codes: [401, 402, 403, 429]
      state-file: data/xai-autoban-state.json
      observe-usage: true
      usage-db-path: /data/usage.sqlite   # 容器内路径示例；按部署调整
```

商店依赖访问 GitHub；API 限流时可改用下方手动安装。

Release ZIP（根目录仅一个二进制）：

| 平台 | 资产名（当前以 linux_amd64 公开发布为主） |
| --- | --- |
| Linux x86_64 | `xai-autoban_{version}_linux_amd64.zip` |

### 手动安装

```text
plugins/linux/amd64/xai-autoban.so
```

```yaml
remote-management:
  secret-key: "你的管理密钥"
  allow-remote: true   # 若 Management 来自其它容器/主机
```

compose 需挂载插件目录与（可选）usage 库，例如：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins
  - ./cpamp-data:/data:ro    # 内含 usage.sqlite
```

### 配置字段摘要

| 字段 | 说明 |
| --- | --- |
| `management-url` | CPA 根地址，**端口必须正确** |
| `management-key` / `management-key-env` | Management 密钥 |
| `disable-hours` | 默认隔离小时数 |
| `class-disable-hours` | 按失败类型覆盖 TTL |
| `status-codes` | 触发隔离的 HTTP 状态码 |
| `classify-body` | 是否解析 body 做失败类型（默认 true） |
| `state-file` | 隔离状态落盘路径 |
| `observe-usage` | 是否读 usage.sqlite（默认 true） |
| `usage-db-path` | sqlite 绝对路径 |
| `auth-dir` | 可选，邮箱 enrich |

日志示例：

```text
pluginhost: plugin registered plugin_id=xai-autoban plugin_name=xai-autoban version=1.4.3
```

---

## Management API（需鉴权）

```text
GET  /v0/management/plugins/xai-autoban/bans
POST /v0/management/plugins/xai-autoban/unban
POST /v0/management/plugins/xai-autoban/unban-all
POST /v0/management/plugins/xai-autoban/delete        # 单条 403
POST /v0/management/plugins/xai-autoban/delete-403    # 全部 403
POST /v0/management/plugins/xai-autoban/import
```

公开资源（面板，无需 Management Key）：

```text
GET /v0/resource/plugins/xai-autoban/status
GET /v0/resource/plugins/xai-autoban/data
GET /v0/resource/plugins/xai-autoban/action?...
```

---

## 构建

```bash
# 需 CGO；本地示例（linux/amd64）
CGO_ENABLED=1 go build -buildmode=c-shared -o dist/xai-autoban.so .

# 或 Docker 多架构（若环境有 docker）
bash build.sh
```

商店兼容包：ZIP 内根目录仅 `xai-autoban.so`，命名  
`xai-autoban_{version}_linux_amd64.zip`。

---

## 行为说明

- 仅自动恢复**本插件成功停用并记入 state** 的账号。
- 不会主动扫全池；只处理已被请求打到的错误凭据。
- Management 暂时不可用时，本地调度隔离仍生效；「当前处置」会显示停用/恢复重试。
- 全部候选都被隔离时，最终行为由 CPA 自带调度决定。

---

## 版本摘要

| 版本 | 要点 |
| --- | --- |
| 1.4.3 | 导出 JSON/CSV（隔离 + state + 用量观测）供本地分析 |
| 1.4.2 | 精简表列；当前处置文案澄清；悬停显示详情 |
| 1.4.1 | 面板「最后使用」「已下线(h)」；state 记 unusable_since（阶梯不重置） |
| 1.4.0 | 失败债务 + 阶梯冷却 + 半开试用；permission 一次硬隔离但非永 ban |
| 1.3.6 | 修复面板 JS 损坏导致一直「正在连接」 |
| 1.3.5 | 永久删除恢复为仅 403；24h 列去掉状态词 |
| 1.3.x | 内嵌 usage.sqlite 观测、圆饼图含正常号 |
| 1.2.x | 失败类型展示与 class TTL |
| 1.1+ | 本 fork 分类与面板增强 |
| 上游 | 调度隔离 + 403 永久删除基线 |

---

## 致谢

- 上游与 CPA 插件生态：[vrxiaojie/xai-autoban](https://github.com/vrxiaojie/xai-autoban)、[akihitohyh/xai-autoban](https://github.com/akihitohyh/xai-autoban)、[ysxk/codex-429-autoban](https://github.com/ysxk/codex-429-autoban)
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) / CPAMP 用量库
- [linux.do](https://linux.do) 社区

## License

[MIT](LICENSE)
