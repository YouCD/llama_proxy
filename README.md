# llama_proxy

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![SQLite](https://img.shields.io/badge/SQLite-local-003B57?logo=sqlite&logoColor=white)](https://www.sqlite.org/)
[![llama.cpp](https://img.shields.io/badge/llama.cpp-local%20LLM-111111)](https://github.com/ggml-org/llama.cpp)
[![SSE](https://img.shields.io/badge/SSE-streaming-1f6feb)](#web-面板)

本地 `llama.cpp` / OpenAI 兼容推理服务器的轻量级反向代理 + 监控面板。

`llama_proxy` 部署在推理服务器前面：记录每一个请求与响应（含原始载荷）、测量 TTFT / 总耗时 / Token 吞吐，并提供一个实时刷新的 Web 面板。除纯转发外，还支持**进程调度模式**——由代理自动启停本地 `llama-server` 进程，按请求体中的模型 ID 区分开发（coding）流量与日常流量，并把本地 background 模型作为动态节点纳入代理池做负载均衡。

![llama_proxy](./docs/logo.svg)

## 截图

仪表盘（中文界面）：

![仪表盘](./docs/1.png)

高级筛选与请求列表：

![请求列表](./docs/2.png)

English UI：

![English dashboard](./docs/3.png)

![English request tape](./docs/4.png)

## 功能

- 反向代理转发到 `llama.cpp` / OpenAI 兼容后端，支持流式（SSE）与非流式响应
- 记录每个请求的原始请求体 / 响应体（按日期 gzip 存储）
- 指标：活跃连接、TTFT、总耗时、prompt/completion Token 数、Token 吞吐（prompt/s、generation/s）、缓存命中率、请求/响应大小、错误
- 多后端加权负载均衡（wrr / swrr / wr / rr / random，基于 [fufuok/balancer](https://github.com/fufuok/balancer)）
- 按请求模型 ID 路由：`llm_prox` 轮询模型列表；具体模型 ID 直连部署该模型的后端/本地进程（启动时校验模型 ID 全局唯一）
- 客户端特征路由规则（`routing`）：按 User-Agent（包含匹配）/ 请求头（精确匹配）把 `llm_prox` 流量固定到后端标签池并池内自动故障转移
- 每后端 `model` 重写与 `api_key` 注入，客户端 key 与后端 key 相互隔离
- 可选进程调度：自动启停本地 `llama-server`，按请求模型 ID 识别 coding 流量，background 模型就绪时作为动态节点入池
- Web 面板（中/英双语）：实时指标卡、进程调度状态、按后端统计、每日统计图、可过滤的请求列表与请求详情
- SQLite（默认）或 PostgreSQL 存储，数据保留期自动清理

## 路由模型

代理按请求体中的 `model` 字段分派流量：

| 请求 model | 去向 |
|---|---|
| `llm_prox`（或省略） | 默认轮询模型列表：命中路由规则时进入规则 pool 子池，否则进入全量代理池（`backends.list` + 就绪的本地节点），按权重策略负载均衡 |
| `scheduling` 条目 key（模型 ID） | 本地调度进程：请求哪个模型 ID 就加载/切换到哪个模型（未就绪时自动拉起并等待；coding 模式续期开发租约；声明了 `gpu` 的 background 在 coding 租约仍活跃时被占卡，返回 503，纯 CPU background 不受影响） |
| `backends.list[].model` | 直连部署该模型的后端（单后端，不故障转移） |
| 未配置的其他模型 ID | 400 错误 |

**模型 ID 唯一性**：启动时校验所有模型 ID（`backends.list[].model` 与 `scheduling` 条目 key）互不重复，且不与 `llm_prox` 冲突；重复时拒绝启动，保证具体模型路由无歧义。

配置 `scheduling` 后进入**进程调度模式**，`llm_prox` 流量的代理池构成：

| 条件 | 代理池 |
|---|---|
| 有就绪的 background 启动集成员（coding 态下本地节点全部出池） | `backends.list` + 本地节点（`local-background`），按权重负载均衡 |
| 当前为 coding 模式 / 本地未就绪 / 进程崩溃 | `backends.list` |

调度细节：

- **模型条目**：`scheduling` 下以模型 ID 为 key（key 即模型 ID，小写生效），每个条目含 `mode`（`coding` 或 `background`）、`command`、`readiness_url`、`api_key`、`log_file`，background 条目另可用 `autostart`/`resident`/`no_pool`/`sleep_timeout`/`gpu`/`weight`/`tags`。coding 与 background 条目均可多个。`lease`/`switch` 是保留 key，不得用作模型 ID。
- **background 模式（启动集 + 按需）**：`autostart: true` 的成员构成**启动集**，代理启动时并行加载（各自独立端口，可各绑一张 GPU）；就绪后以条目 `weight`（缺省 1）作为名为 `local-background` 的动态节点加入代理池参与负载均衡；探测失败或进程意外退出时自动出池，恢复就绪后重新入池。未标 `autostart` 的 background **不随启动加载**，收到其模型 ID 请求时才单独拉起，与运行中的成员并行运行（不打断它们）。
- **coding 模式（按需）**：收到流量才切换；coding 态下只停**声明了 `gpu` 的** background 成员让显存，未声明 `gpu` 的纯 CPU 成员与 `resident` 成员保留运行；距最后一次 coding 模型流量超过 `scheduling.lease.coding_idle_timeout`（例 30m）后，代理优雅停掉 coding 进程并切回 **background 启动集**，避免大模型常驻占用显存。
- **`gpu`（background 可选）**：声明该进程占用的 GPU（如 `"CUDA1"`，大小写不敏感），调度器据此做 **per-GPU 独占仲裁**：同一 `gpu` 上的非 `resident` 模型运行时互斥换入——请求其中一方时先把同卡另一方优雅停掉（排空在途、等显存释放）再加载，因此同卡模型**可共用同一端口**；启动集内两个成员不能声明同一 `gpu`；`resident` 模型的 `gpu` 被独占钉死，其他模型声明同卡时配置校验直接报错。未声明 = 纯 CPU（不占显存，不参与仲裁）。
- **`resident`（background 可选）**：coding 切换时**保留**该 background 成员（不被杀掉、PID 不变），coding 态下仍可直连；coding 空闲切回 background 组时也不重启它。主要用于 GPU 模型显式声明“与 coding 共存”（并因此钉死该卡）。**纯 CPU 模型（未声明 `gpu`）不占显存，coding 切换天然不停它，无需此标记**。适合需要与 coding 模型并存于显存、且切换时不想反复加载的大模型。
- **`no_pool`（background 可选）**：该模型**不入代理池**（不被 `llm_prox` 默认流量轮询命中），仅当请求体 `model` 恰好是它的模型 ID 时直连。适合翻译 / Embedding / 分类等不应混入通用均衡流量的**专用模型**。与 `resident` 正交：`resident` 决定 coding 切换时是否保留，`no_pool` 决定是否入池。
- **`sleep_timeout`（background 可选，需配合 `no_pool`）**：该成员**无在途请求**、且距最近一次请求（或首次就绪）超过该时长后，调度器**主动停其进程释放显存**（休眠）；下次请求该模型 ID 时按需重新加载唤醒。`0`（缺省）禁用休眠，成员就绪后永久常驻。**必须同时设 `no_pool`**：休眠判定依赖"全部流量都是按模型 ID 的直连请求"（可被代理层完整观测）；池内成员还会被 `llm_prox` 轮询命中（不经过休眠观测点），闲置判定会失真，故禁止对池内成员配置休眠。休眠成员不出现在代理池、不占显存，组切换也不打扰它；只有直接请求该模型 ID 才会唤醒。
- **切换**：停旧进程前按 `switch.drain_timeout` 优雅排空在途请求，`kill_timeout` 内未退出则强杀；新进程按 `startup_timeout` 等待就绪。
- **优先级抢占**：coding > background。background 加载（或空闲切回）期间收到 coding 模型流量时，coding 切换直接抢占在途的 background 切换（取消其就绪等待）并立即开始加载 coding，而不是排队等 background 加载完成；启动时首次 background 加载被抢占不视为致命错误。反之，coding 租约仍活跃时，声明了 `gpu` 的非常驻 background 模型请求被拒（503），不会杀掉正在使用的 coding 进程（纯 CPU background 不受影响，照常服务）。

### 路由规则（routing）

路由规则只作用于 `llm_prox`（默认池）流量，具体模型 ID 直连不受规则影响。规则把特定客户端特征的请求固定到指定标签池：

- 规则按配置顺序评估，**首条命中生效**；
- 每条规则的 `header` 中所有请求头都必须匹配（AND）：**User-Agent 采用包含匹配**（不区分大小写的子串包含，配置 `GoClaw` 可命中 `GoClaw/2.1 (Windows)`），其余请求头 trim 后精确相等、区分大小写；
- 命中后请求只从 `tags` 含 `pool` 标签的后端中选择（本地 background 节点可用 `scheduling` 条目的 `tags` 携带标签）；请求失败时自动转移到同一标签池内下一个未尝试的后端；
- 未配置 `routing` 时不存在路由规则，全部流量走默认池。

```yaml
routing:
  rules:
    - name: "goclaw"
      header:
        User-Agent: "GoClaw"   # UA 包含该子串即命中（不区分大小写）
      pool: "tool_call"
    - name: "agent-x"              # 多条件 AND：User-Agent 包含 + X-Agent-Env 精确匹配
      header:
        User-Agent: "AgentX"
        X-Agent-Env: "prod"
      pool: "fast"

backends:
  list:
    - name: "gpu-server-1"
      url: "http://gpu-server-1:8080"
      tags: ["fast"]             # 加入 fast 标签池
    - name: "gpu-server-2"
      url: "http://gpu-server-2:8080"
      tags: ["tool_call"]        # 加入 tool_call 子池
```

## Quick Start

### 方式一：Docker Compose（推荐）

仓库已自带 `docker-compose.yml`（端口 9091，`./data` 持久化，`./config.yaml` 挂载）。

1. 准备 `config.yaml`（最小示例）：

```yaml
server:
  listen_addr: ":9091"
  data_dir: "./data"

database:
  type: "sqlite"

backends:
  list:
    - name: "backend-1"
      url: "http://host.docker.internal:8080"
      weight: 1
      enabled: true
```

2. 启动：

```bash
docker compose up -d --build
```

3. 打开面板：

```text
http://localhost:9091/_proxy/ui
```

4. 把客户端指向代理（而不是直接指向 `llama.cpp`）：

- 原来：`http://localhost:8080`
- 现在：`http://localhost:9091`

### 方式二：本地构建运行

```bash
# 纯二进制（前端从 web/ 目录读取，部署需带上 web/）
make build
./bin/llama_proxy -f config.yaml

# 或嵌入前端单文件
make build-embed
./bin/llama_proxy -f config.yaml

# 快速开发
go run . -f config.yaml
```

`-f` 缺省读取当前目录的 `config.yaml`。

### 方式三：GHCR 预构建镜像

每次 push 到 `main` 及每个 `v*` tag 都会自动构建多架构镜像并推送到 **GHCR**（`ghcr.io/youcd/llama.cpp-router-monitor`）。

| Tag | 说明 |
|---|---|
| `next` | `main` 分支最新构建 |
| `sha-<commit>` | 指定 commit 的构建 |
| `1.2.3`, `1.2` | 来自 `v1.2.3` 等 tag 的正式版本 |
| `latest` | 最近一个 `v*` release |

```bash
docker pull ghcr.io/youcd/llama.cpp-router-monitor:next

docker run -d --name llama-cpp-router-monitor \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v $(pwd)/data:/app/data \
  -p 9091:9091 \
  --restart unless-stopped \
  ghcr.io/youcd/llama.cpp-router-monitor:next
```

> 镜像已内嵌前端，无需单独的 `web/` 目录；容器内二进制位于 `/app/llama_proxy`。
> GHCR 包默认私有，其他机器拉取请先在 GitHub 包设置中公开，或 `docker login ghcr.io` 使用带 `read:packages` 权限的 token。

### 自行构建镜像

Dockerfile 为多阶段自包含构建（构建前端 + 编译嵌入二进制）：

```bash
docker compose up -d --build
# 或
docker build -t llama-cpp-router-monitor .
```

> 国内构建拉取 Go 模块 / npm 包可能失败，可传入镜像参数（`docker compose build` 同样生效）：

```bash
docker build \
  --build-arg GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct \
  -t llama-cpp-router-monitor .
```

## 配置参考

完整示例见 [config.example.yaml](./config.example.yaml)，最小配置只需 `server` + `backends.list`。

```yaml
server:
  listen_addr: ":9091"
  data_dir: "./data"
  # 面板 Host 白名单（忽略端口与大小写），留空不限制；不在列表内的 Host 访问面板返回 403
  ui_allowed_hosts:
    - "llm.youcd.online"  # 忽略端口，"llm.youcd.online:9091" 也命中

database:
  type: "sqlite"                # sqlite | postgresql
  sqlite:
    path: "proxy.db"
  postgresql:
    dsn: "postgres://user:password@localhost:5432/proxy?sslmode=disable"
    max_open_conns: 25
    max_idle_conns: 5
    conn_max_lifetime_seconds: 300

backends:
  strategy: "wrr"               # wrr | swrr | wr | rr | random
  list:
    - name: "gpu-server-1"
      url: "http://gpu-server-1:8080"
      weight: 50
      enabled: true
      model: "qwen"             # 后端实际部署的模型 ID，转发时自动重写请求体 model 字段
      api_key: "secret-1"       # 后端 API Key，转发时自动注入 Authorization: Bearer <key>
      # tags: ["fast"]          # 可选：加入的路由标签池，routing 规则按 pool 标签选择该后端；
                                 # 含 "tool_call" 即加入 tool_call 子池
    - name: "gpu-server-2"
      url: "http://gpu-server-2:8080"
      weight: 30
      enabled: true
      model: "deepseek"
      api_key: "secret-2"

# 路由规则（可选）：把特定客户端特征的请求固定到指定标签池，首条命中生效；
# 未配置时不存在路由规则，全部流量走默认池
#routing:
#  rules:
#    - name: "goclaw"
#      header:
#        User-Agent: "GoClaw"   # UA 包含该子串即命中（不区分大小写）
#      pool: "tool_call"

proxy:
  api_key: "client-secret"      # 客户端访问代理的 API Key（与后端 api_key 隔离），留空则不鉴权
  retention_days: 14            # 数据保留天数，0 或负数表示不清理
  max_request_bytes: 33554432   # 32MB
  max_capture_bytes: 33554432   # 32MB
  request_timeout_seconds: 600
  poll_backend_metrics: true    # 定期拉取后端 /metrics 存入历史
  poll_interval_seconds: 10
  # 白名单：仅这些路径被转发记录，其余一律 404（精确或前缀匹配）；未配置时默认 OpenAI 兼容接口
  record_paths:
    - "/v1/chat/completions"
    - "/v1/completions"
    - "/v1/embeddings"

# 进程调度（可选）：配置后启用模型 ID 驱动的本地进程管理，未配置则保持纯转发模式。
# 条目 key 即模型 ID（小写生效）：请求体 model 为该值即路由到该进程。
# 配置了 command 的条目 readiness_url 必填；所有模型 ID 启动时校验全局唯一，且不与 llm_prox 冲突。
scheduling:
  # coding 模型（按需）：收到流量才加载，空闲租约到期后停掉切回 background 启动集。
  # coding 与 background 可共用同一端口（coding 运行期间占卡的 background 已停止）。
  qwen3.8:
    mode: coding
    # command 支持多行：每个参数一行，拼接为一行命令执行（块内不可用 # 注释）
    command: >
      /usr/local/bin/llama-server
      -m /path/to/coding.gguf
      --host 0.0.0.0 --port 8080 -c 8192
      --alias qwen3.8
    readiness_url: "http://127.0.0.1:8080/v1/models"  # 就绪探测地址（完全按此配置 GET）
    api_key: "your-api-key"       # 模型进程自身的 API Key（对应 llama-server --api-key）
    log_file: "./data/coding.log" # 可选：该进程 stdout/stderr 日志
  # background 启动集成员：autostart: true，代理启动时并行拉起。
  qwen3.6:
    mode: background
    command: >
      /usr/local/bin/llama-server
      -m /path/to/background.gguf
      --device CUDA0 --host 0.0.0.0 --port 8080 -c 8192
      --alias qwen3.6
    autostart: true                     # 启动集成员：代理启动时拉起；不加则按需加载
    gpu: "CUDA0"                        # 声明占用的 GPU：调度器据此做同卡互斥换入仲裁
    readiness_url: "http://127.0.0.1:8080/v1/models"
    api_key: "your-api-key"
    weight: 1   # 就绪后作为动态节点加入代理池的权重，与 backends.list 一起负载均衡，缺省 1
    tags: ["fast"]
    log_file: "./data/background.log"
  # 纯 CPU 专用翻译模型：不占显存（未声明 gpu），coding 切换天然不停它，无需 resident；
  # 不入代理池、闲置休眠。no_pool 使其仅被 model="translator" 的直连请求命中；
  # sleep_timeout 使其无在途请求且闲置超过 30m 后自动停进程释放内存，下次请求按需唤醒。
  translator:
    mode: background
    command: >
      /usr/local/bin/llama-server
      -m /path/to/translator.gguf
      --device none --host 0.0.0.0 --port 8082 -c 8192
    autostart: true                      # 启动集成员：代理启动时拉起
    readiness_url: "http://127.0.0.1:8082/v1/models"
    no_pool: true                        # 不入代理池，仅直连命中
    sleep_timeout: 30m                   # 闲置 30m 且无在途请求后自动休眠（需配合 no_pool）
    log_file: "./data/translator.log"
  lease:
    coding_idle_timeout: 30m  # 距最后一次 coding 流量超过该时长即切回 background 启动集
  switch:
    drain_timeout: 10s        # 切换前优雅排空在途请求的等待上限
    kill_timeout: 10s         # 停止旧进程的等待上限
    startup_timeout: 120s     # 等待新模型加载就绪的上限
```

### API Key 隔离

代理同时管理两套互不依赖的 key：

```
客户端 ── Bearer <client_key> ──► llama_proxy ── Bearer <backend_key> ──► 后端 LLM
```

- `proxy.api_key`：客户端访问代理的 key，配置后所有代理转发请求必须携带 `Authorization: Bearer <key>`，否则 401；留空则不做客户端鉴权。
- `backends.list[].api_key` / `scheduling.*.api_key`：转发到对应后端时自动注入该后端的 key；未配置则客户端的 `Authorization` 头原样透传。
- `backends.list[].model`：配置后转发前自动重写请求体中的 `model` 字段为该后端实际部署的模型 ID，客户端无需关心各后端模型名不一致的问题。

## Web 面板

访问 `http://<host>:<port>/_proxy/ui`（中/英双语，右上角切换），通过 SSE 实时推送更新，支持自动刷新：

- **实时指标卡**：活跃连接、每小时请求数、生成速度（token/s）、平均 TTFT、LLM 错误率、总请求数、总 Token 数
- **进程调度面板**（调度模式）：当前进程（coding / background）、就绪状态、PID、服务地址、租约时长、剩余租约、已空闲时长
- **按后端统计**：每个后端的请求量、平均 TTFT、TOKEN/S、错误率
- **每日统计图**：每日 Token 用量（prompt/completion/total）、每日请求状态分布（200/4xx/5xx）、每日请求总数
- **请求列表**：时间、路径、客户端（IP）、User-Agent、提供商、状态、模型、耗时（TTFT/总计）、Token（提示/补全）、缓存命中率、prompt/s；支持分页加载
- **筛选**：状态码、时间范围（`time_from`/`time_to`）、路径、模型、后端、方法、客户端 IP、User-Agent（`user_agent` 子串匹配）、流式/非流式、仅失败、有 Token、仅对话补全
- **请求详情**：查看原始请求/响应载荷（raw）、删除记录

## API 端点

监控端点均位于 `/_proxy` 前缀下，其余路径按代理转发处理（`/v1/models` 由代理自身应答：返回 `llm_prox` 与全部已配置的具体模型 ID，不转发）。

```text
GET    /_proxy/                          服务信息与端点列表
GET    /_proxy/health                    健康检查
GET    /_proxy/scheduler                 调度器状态（未启用调度时 404）
GET    /_proxy/live                      实时活跃连接数
GET    /_proxy/stats?hours=24            时间窗口聚合统计
GET    /_proxy/stats-by-backend?hours=24 按后端统计
GET    /_proxy/daily-stats?days=30       每日统计
GET    /_proxy/requests?limit=100&offset=0  请求列表
GET    /_proxy/request/{id}              请求详情
DELETE /_proxy/request/{id}              删除请求（含原始载荷）
GET    /_proxy/raw/{id}/{request|response}  原始载荷
GET    /_proxy/events                    事件流（SSE）
GET    /_proxy/backend-metrics?limit=200 后端 /metrics 历史
GET    /_proxy/models                    历史出现过的模型列表
GET    /_proxy/backends                  后端列表
GET    /_proxy/ui                        Web 面板
```

`/_proxy/requests`、`/_proxy/stats`、`/_proxy/stats-by-backend`、`/_proxy/daily-stats` 支持的过滤参数：

| 参数 | 说明 |
|---|---|
| `q` | 关键词搜索（匹配 id/路径/query/客户端 IP/模型/错误信息） |
| `path` / `model` / `backend` / `method` | 精确/前缀匹配过滤 |
| `status` | 状态码过滤：数字精确匹配（如 `200`），或状态码段 `4xx`（400–499）/ `5xx`（≥500） |
| `time_from` / `time_to` | 绝对时间窗口（RFC3339 或本地时间写法，如 `2026-09-13T00:00:00+08:00`）；省略时回退到 `hours`/`days` |
| `stream` | `true`/`false` 过滤流式/非流式请求 |
| `errors_only` | 仅失败请求 |
| `with_tokens` | 仅带 Token 统计的请求 |
| `chat_completions_only` | 仅 `/v1/chat/completions` 请求 |

示例：

```bash
curl http://localhost:9091/_proxy/stats?hours=24
curl "http://localhost:9091/_proxy/requests?limit=50&errors_only=true"
```

代理转发示例：

```bash
curl http://localhost:9091/v1/chat/completions \
  -H "Authorization: Bearer client-secret" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.8","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 数据存储

本地数据保存在 `server.data_dir`（默认 `./data`）：

- `proxy.db` — SQLite 数据库（数据库类型与路径由 `database` 段控制）
- `raw/YYYY-MM-DD/*.gz` — 按日期分组的原始请求/响应载荷

超过 `proxy.retention_days` 的数据自动清理；`0` 或负数表示永久保留。

### PostgreSQL

默认使用 SQLite。切换 PostgreSQL：

```yaml
database:
  type: postgresql
  postgresql:
    dsn: "postgres://user:password@localhost:5432/proxy?sslmode=disable"
```

启动时代理会：数据库不存在时自动创建（连接 `postgres` 维护库执行 `CREATE DATABASE`，需要 `CREATEDB` 权限）；自动建表与索引（`CREATE TABLE IF NOT EXISTS`）。

## 隐私

面向本地部署设计：请求与响应全部落在本机 `data_dir`，不配置外网暴露时数据不会离开你的机器。面板可通过 `server.ui_allowed_hosts` 限制可访问的 Host。

## 资源占用建议

- `proxy.max_capture_bytes` 保持在 8MB~32MB 之间
- 不需要后端 metrics 历史时关闭 `proxy.poll_backend_metrics`
- `/metrics` 无需频繁采集时调大 `proxy.poll_interval_seconds`
- 保留期较高时留意 raw 载荷目录的磁盘占用

## 限制

- Token 与部分计时字段依赖后端实际返回的内容
- raw 载荷在长保留期下会占用可观磁盘空间
- 定位是轻量级本地代理，不是完整的可观测性平台

## License

MIT License，见 [LICENSE](./LICENSE)。
