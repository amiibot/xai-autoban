# xai-autoban

`xai-autoban` 是一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 原生插件，用于自动隔离持续返回错误的 xAI OAuth 凭据，避免 CPA 在大号池中逐个重试无效账号而显著拉长首 token 时间。

## 功能

| 上游状态 | 默认处理 |
| --- | --- |
| `401` | 通过 Management API 停用 24 小时 |
| `402` | 通过 Management API 停用 24 小时 |
| `403` | 通过 Management API 停用 24 小时 |
| `429` | 通过 Management API 停用 24 小时（默认；可用 class 覆盖） |

### 细分类（v1.1，本 fork）

失败时解析 `UsageFailure.Body`，将账号归入 class，并按 class 决定隔离时长与是否允许永久删除：

| Class | 典型信号 | 默认隔离 | 默认可永久删除 |
| --- | --- | --- | --- |
| `auth` | 401；403 body 含 invalid/expired token | 24h | 否 |
| `payment` | 402 | 24h | 否 |
| `quota_paid` | spending-limit 等 | 24h | 否 |
| `quota_free` | free-usage-exhausted 等 | 24h | 否 |
| `permission` | permission-denied / chat endpoint denied | 24h | **是** |
| `rate_limit` | 429 | 24h | 否 |
| `forbidden_unknown` | 其它 403 | 24h | 否 |

配置示例：

```yaml
plugins:
  configs:
    xai-autoban:
      enabled: true
      disable-hours: 24
      classify-body: true
      # class-disable-hours:
      #   rate_limit: 24
      #   permission: 48
      deletable-classes: [permission]   # 可追加如 quota_free
```

- 面板支持按 Class 展示；永久删除仅 `deletable-classes`（默认只有 `permission`），也可多选 class 批量删除。
- 插件 id / 文件名仍为 `xai-autoban`，与 CPA 原安装路径兼容。

### 用量观测与圆饼图（v1.2）

- **不预 ban**：不会因为用量高或 grok-quota 标记而自动隔离。
- 若存在 `plugins/grok-quota-state.json`（或配置 `quota-state-file` / 环境变量 `GROK_QUOTA_STATE_PATH`），面板为**已隔离**行附加只读 `tokens_24h`。
- 观测页展示两个圆饼图：账号池 **HTTP 状态**（含**正常** + 401/402/403/429）与 **Class**（含正常 + permission/quota…）。池规模优先 Management 列表，其次 grok-quota-state。
- 推荐与 [grok-quota](https://github.com/gzy3894-png/grok-quota) **并排安装**；本插件只读其状态文件。


- 只处理 `xai` provider，不影响 Codex、Claude、Gemini 等其他凭据。
- 错误发生后立即在 CPA 调度阶段跳过该凭据，后台再调用网页 Management API 真正停用账号。
- 24 小时到期后调用 Management API 重新启用账号；失败时自动重试。
- 状态落盘，CPA 重启后仍会继续执行未完成的停用和恢复任务。
- Management Key 错误时启用全局冷却，避免连续错误请求触发 CPA 的管理接口 IP 封禁。
- 可通过 `status-codes` 自定义触发停用的 HTTP 状态码。
- 提供实时统计、搜索、筛选、分页和批量解禁界面。
- 支持单个、选中项、按状态码和全部解禁。
- 支持对 **可删 class**（默认 `permission`）永久删除；可配置 `deletable-classes` 勾选多种 class（调用 Management API 删除凭据文件，不是解除禁用）。
- 保留带 CPA 管理密钥保护的 Management API。

## 构建

构建脚本使用官方 Debian Go 镜像，需要 Docker，并会运行测试后分别编译 Linux arm64 和 amd64：

```bash
bash build.sh
```

产物：

```text
dist/xai-autoban-linux-arm64.so
dist/xai-autoban-linux-amd64.so
```

## 安装

### 推荐：通过 CPA 插件商店安装

在 CPA 的 `config.yaml` 中把本仓库的 `registry.json` 添加为插件商店源：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/vrxiaojie/xai-autoban/main/registry.json"
  configs:
    xai-autoban:
      enabled: true
      priority: 200
      management-url: http://127.0.0.1:8317
      management-key-env: CPA_MANAGEMENT_KEY
      disable-hours: 24
      status-codes: [401, 402, 403, 429]
      state-file: data/xai-autoban-state.json
```

保存配置并重启 CPA 后，打开 **管理中心 → 插件商店**，找到 **xAI Autoban** 并点击安装。商店会根据 CPA 所在平台自动下载对应的 GitHub Release ZIP。

商店安装依赖 CPA 能访问 `raw.githubusercontent.com`、`api.github.com` 和 GitHub Release 下载地址。Release 资产支持：

| 平台 | Release 资产 |
| --- | --- |
| Windows x86_64 | `xai-autoban_{version}_windows_amd64.zip` |
| Linux x86_64 | `xai-autoban_{version}_linux_amd64.zip` |
| Linux ARM64 | `xai-autoban_{version}_linux_arm64.zip` |
| macOS Intel | `xai-autoban_{version}_darwin_amd64.zip` |
| macOS Apple Silicon | `xai-autoban_{version}_darwin_arm64.zip` |

每个 ZIP 的根目录都只包含平台对应的 `xai-autoban.so`、`xai-autoban.dll` 或 `xai-autoban.dylib`，Release 同时提供 `checksums.txt` 供 CPA 校验。

### 手动安装

下载与服务器架构对应的文件后，必须将文件名改为 `xai-autoban.so`，再复制到 CPA 插件目录：

```text
amd64: plugins/linux/amd64/xai-autoban.so
arm64: plugins/linux/arm64/xai-autoban.so
```

CPA 必须启用 Management API，并配置管理密钥：

```yaml
remote-management:
  secret-key: "你的管理密钥"
```

### 配置方式

安装插件后，可以通过下面两种方式完成启用与参数配置，任选其一即可。

| 方式 | 适用场景 |
| --- | --- |
| 编辑 `config.yaml` | 适合批量部署、脚本化运维、需要配置完整高级参数 |
| CPA 插件管理界面 | 适合已在管理后台操作，不想改配置文件 |

两种方式使用的是同一套插件配置字段；保存后由 CPA 加载并交给本插件解析。若你同时改了文件和管理界面，请以当前 CPA 版本的最终生效配置为准。

#### 方式一：编辑 `config.yaml`

在 CPA 的 `config.yaml` 中启用并配置插件：

```yaml
plugins:
  enabled: true
  configs:
  # 插件名称需要和安装后的 .so 文件名一致
    xai-autoban:
      enabled: true
      priority: 200
      management-url: http://127.0.0.1:8317
      management-key-env: 你的管理密钥 # secret-key
      disable-hours: 24
      status-codes: [401, 402, 403, 429]
      request-timeout-seconds: 10
      retry-interval-seconds: 60
      auth-failure-cooldown-seconds: 600
      state-file: data/xai-autoban-state.json
```

也可以使用 `management-key` 直接配置密钥，但不建议把明文密钥提交到 Git。`state-file` 所在目录必须允许 CPA 进程写入。

#### 方式二：在 CPA 插件管理中配置

1. 将 `.so` 安装到对应架构目录后，启动或重启 CLIProxyAPI，确认插件已加载。
2. 打开 CPA 管理后台中的插件管理页面。
3. 找到 `xai-autoban`，在表单中填写配置并保存。插件会声明常用字段，例如：
   - `management-url`
   - `management-key` / `management-key-env`
   - `disable-hours`
   - `status-codes`
   - `state-file`
4. 如需 `request-timeout-seconds`、`retry-interval-seconds`、`auth-failure-cooldown-seconds` 等高级项，可在 `config.yaml` 中补充；`enabled`、`priority` 等宿主级开关也以 CPA 插件管理或 `config.yaml` 中的插件宿主配置为准。
5. 保存后如 CPA 要求，再重启一次使配置生效。

重启 CLIProxyAPI 后，日志应包含：

```text
pluginhost: plugin registered plugin_id=xai-autoban plugin_name=xai-autoban version=1.1.0
```

## 管理面板
带 CPA 管理鉴权的兼容 API：

```text
GET  /v0/management/plugins/xai-autoban/bans
POST /v0/management/plugins/xai-autoban/unban
POST /v0/management/plugins/xai-autoban/unban-all
POST /v0/management/plugins/xai-autoban/delete
POST /v0/management/plugins/xai-autoban/delete-403
POST /v0/management/plugins/xai-autoban/import
```

## 状态说明

- 插件只会自动恢复由自己成功停用并记录的账号。
- `401`、`402`、`403`、`429` 默认在首次错误后按 class 隔离 24 小时；可用 `disable-hours` / `class-disable-hours` 调整。
- 凭据修复、充值或恢复订阅后，可从面板手动解禁，无需等待到期。
- 插件仅处理已经被请求命中的错误凭据，不会主动扫描整个账号池。
- 如果所有候选凭据都被插件隔离，最终行为交由 CPA 自带调度器决定。
- Management API 暂时不可用时，本地调度隔离仍然生效；面板会显示停用或恢复重试状态。

## 致谢

- 本项目基于 [akihitohyh/xai-autoban](https://github.com/akihitohyh/xai-autoban)，并参考 [ysxk/codex-429-autoban](https://github.com/ysxk/codex-429-autoban) 的 CPA 插件 ABI 实现思路。
- 感谢 [linux.do](https://linux.do) 社区的讨论、反馈与支持。

## License

[MIT](LICENSE)
