# New API 业务端接入指南

业务系统用生成密钥提交图片、视频或音频任务，收到任务 ID 后结束当前 HTTP 请求，再查询状态、下载结果。New API 负责渠道调用、现有渠道重试、计费及私有 R2 归档；业务系统负责自己的用户权限、任务页面、业务重试决策和长期资产保存。

本文对应本仓库的业务密钥与异步接口，包括保存期更新：新任务默认保存 7 天，可指定 600 至 2592000 秒，即 10 分钟至 30 天，从生成完成时开始计时。部署旧版本不能只靠传入新参数获得这些能力。底层配置和验证记录见 [异步接口与部署说明](ASYNC_BUSINESS_API.md)，容器模板见 [compose.async.yml](compose.async.yml)。

## 接入前提

- New API 已配置可用模型、供应商渠道及价格，业务账户有可用额度。
- 新异步接口已配置私有 R2 的读写删除凭据；不支持本地持久化或上游临时链接兜底。业务端不需要、也不应获得 R2 管理密钥。
- 当前异步实现要求关闭 Redis 额度缓存和批量额度更新，日志库使用 SQLite、MySQL 或 PostgreSQL，不使用 ClickHouse。对应配置错误会返回 503，不影响原同步接口。
- 网关入口使用 HTTPS。任务提交仍需上传输入并写入数据库，虽然不等待生图完成，但大文件、慢上传或文件下载仍可能超时；异步不是所有代理超时的保证。
- 下方 cURL 示例使用 Bash 续行语法，域名、模型、密钥和文件名均为占位值。不要把真实密钥写进仓库、前端代码、网址查询参数或共享日志。

建议的业务流程是：业务后端先保存自己的业务任务与幂等键，提交至 New API，将返回的 `async_...` ID 持久化关联，再向自己的前端返回任务 ID。轮询由业务后台任务或前端访问业务后端完成，不要在业务 HTTP 请求里持续等待数分钟。

## 账户和密钥分工

一级业务系统 A 使用独立 New API 账户。A 为子系统 B 创建生成密钥，并可为该密钥设置一级标签和二级标签。例如一级标签为 `subsystem-b`，二级标签为 `product-image`；这只是业务约定，标签不强制表示系统或产品，也不要求两级同时填写。

| 凭据 | 使用位置 | 能力 |
| --- | --- | --- |
| 管理访问令牌 `nap_...` | A 的可信管理后端，调用 `/api/*` | 按授予的 scope 管理本账户密钥、查看本账户任务和消费 |
| 生成密钥 `sk-...` | B 的可信业务后端，调用 `/v1/*` | 按额度、模型、模态等限制生成；读取同账户和同标签域内的结果 |
| R2 凭据 | 仅 New API 部署端 | 保存、读取和清理私有对象，不交给 A 或 B 的客户端 |

管理访问令牌通过该账户的浏览器登录会话创建；生成密钥不能创建其他密钥，也不能调用管理接口。当前版本从管理令牌确认账户身份，不要求 `New-Api-User` 请求头。授权 scope 精确匹配，`write` 不会自动包含 `read` 或 `reveal`。

| 操作 | 管理令牌需要的 scope |
| --- | --- |
| 查看业务账户标识 | `profile:read` |
| 修改业务账户标识 | `profile:write` |
| 查看密钥列表及配置 | `api_key:read` |
| 创建业务密钥并获取完整密钥 | `api_key:write` 和 `api_key:reveal` |
| 更新、禁用或删除密钥 | `api_key:write` |
| 查看本账户任务、结果、文件和消费日志 | `usage:read` |

只为管理后端授予所需 scope，不要把管理令牌下发给子系统或浏览器。管理接口限制在令牌所属账户内，即使管理员也不能用这些 owner 接口读取另一个账户的结果。

### 标识和访问域

| 字段 | 设置位置 | 含义 |
| --- | --- | --- |
| `business_system_id` | 账户业务资料 | 一级业务系统标识，最多 128 个 Unicode 字符 |
| `tag_level_1`、`tag_level_2` | 生成密钥 | 各最多 64 个 Unicode 字符；两级分别可选，用于结果访问域及统计 |
| `business_id` | 每次提交 | 订单、项目等业务关联标识，最多 128 个 Unicode 字符 |
| `external_user_id` | 每次提交 | 业务自己的用户标识，最多 128 个 Unicode 字符 |
| `external_task_id` | 每次提交 | 业务自己的任务标识，最多 128 个 Unicode 字符 |
| `id`、`async_task_id` | 网关返回 | New API 异步任务标识，业务端应保存并用于查询及消费关联 |

业务标识会去掉首尾空白，不允许控制字符。结果访问域是 **New API 账户 ID + 一级标签 + 二级标签**，区分大小写；空标签是具体值，不是通配符。同账户下两级都为空的密钥共享无标签域。

同账户同标签的新密钥可以读取旧密钥提交的未过期结果。修改标签会改变该密钥的访问范围，不会迁移旧任务和消费记录；历史记录保存提交时快照。`business_system_id` 和三种逐次业务标识只是归因字段，不改变归属或权限。

特别注意：`external_user_id` 不是用户级访问控制。同一个 B 密钥能读取该标签域内不同用户的结果。B 必须在自己的后端验证登录用户是否拥有对应业务任务，不能仅凭前端传来的用户 ID、标签或网关任务 ID 放行。

## 创建与管理业务密钥

先设置账户的一级业务系统标识：

```bash
curl -sS -X PUT 'https://gateway.example.com/api/business/profile' \
  -H 'Authorization: Bearer <management-access-token>' \
  -H 'Content-Type: application/json' \
  --data '{"business_system_id":"system-a"}'
```

为子系统创建只允许图片生成的密钥：

```bash
curl -sS -X POST 'https://gateway.example.com/api/business/keys' \
  -H 'Authorization: Bearer <management-access-token>' \
  -H 'Content-Type: application/json' \
  --data '{
    "name": "subsystem-b-image",
    "expired_time": -1,
    "remain_quota": 100000,
    "unlimited_quota": false,
    "model_limits_enabled": true,
    "model_limits": "your-configured-image-model",
    "tag_level_1": "subsystem-b",
    "tag_level_2": "product-image",
    "allowed_modalities": ["image"]
  }'
```

`remain_quota` 使用 New API 内部额度单位，不是人民币、美元或生成次数；应按实际价格和账户配置设置。`expired_time` 为 Unix 秒，`-1` 表示不过期。`model_limits` 是逗号分隔的模型名称字符串，不是数组；需要多模型时填写配置中实际可用的名称。

`allowed_modalities` 可选值为 `text`、`image`、`video`、`audio`。省略或 `[]` 表示不限制模态，**不是禁止生成**；要停用密钥应禁用其状态。模型、模态、额度和期限限制同时生效。

创建成功响应节选：

```json
{
  "success": true,
  "message": "",
  "data": {
    "id": 123,
    "key": "sk-<generated-secret>",
    "token": {
      "id": 123,
      "tag_level_1": "subsystem-b",
      "tag_level_2": "product-image",
      "allowed_modalities": ["image"]
    }
  }
}
```

完整生成密钥取 `data.key`，不是 `data.token.key`，后者为脱敏值。立即存入业务后端的密钥管理设施，不要打印完整创建响应。`GET /api/business/keys/:id` 只返回脱敏配置，不用于重新取出完整密钥。

账户资料、密钥、日志等管理 API 使用 `{success,message,data}` 包装，业务错误可能仍为 HTTP 200 且 `success:false`，必须同时检查 HTTP 状态和 `success`。`/api/business/tasks/...` 是例外，与 `/v1/async/tasks/...` 的读取形状一致：任务查询返回直接任务对象，结果返回归档 JSON，文件返回原始字节。生成接口与异步接口不要套用普通管理 API 的包装解析方式。

### 更新与停用

普通更新使用 `PUT /api/business/keys`，密钥 ID 放在 JSON 的 `id` 中，不是 `PUT /api/business/keys/:id`。这不是通用 PATCH：`name`、`expired_time`、`remain_quota`、额度模式、模型限制、IP 限制、分组等传统字段会按传入值更新。先读取配置并构造完整编辑请求，不要仅传 `{id,tag_level_1}`，避免把其他配置重置；并发消费期间也不要用过时的额度快照覆盖余额。

只有新增标签和模态字段支持省略保留原值：空字符串显式清空标签，`allowed_modalities:[]` 清除模态限制，`null` 与省略一样保留。改标签之前确认密钥的旧结果读取范围会变化。

只停用密钥时，用状态专用请求，不需要回传其他配置：

```bash
curl -sS -X PUT 'https://gateway.example.com/api/business/keys?status_only=true' \
  -H 'Authorization: Bearer <management-access-token>' \
  -H 'Content-Type: application/json' \
  --data '{"id":123,"status":2}'
```

`status:1` 为启用，`status:2` 为禁用；重新启用仍受期限、额度等校验。停用不等于取消已经发给供应商的生成。禁用或删除后不能再用原密钥读取结果，可由本账户管理端或同账户同标签的替代密钥读取未过期结果。

## 提交图片任务

```bash
curl -sS -i -X POST 'https://gateway.example.com/v1/async/tasks' \
  -H 'Authorization: Bearer <generation-key>' \
  -H 'Idempotency-Key: b-order-0001-attempt-1' \
  -H 'Content-Type: application/json' \
  --data '{
    "endpoint": "/v1/images/generations",
    "retention_seconds": 604800,
    "business_id": "order-0001",
    "external_user_id": "customer-8",
    "external_task_id": "b-task-100",
    "request": {
      "model": "your-configured-image-model",
      "prompt": "A landscape with mountains and a lake",
      "n": 1
    }
  }'
```

`request` 是原生生成参数对象，模型在 `request.model` 内。`endpoint`、保存期和业务归因是外层字段，不要放入 `request`，也不要在逐次提交中尝试覆盖密钥标签或账户归属。

成功返回 **HTTP 202**，`Location` 为 `/v1/async/tasks/<id>`；响应直接是任务对象，没有 `success` 或外层 `data`，例如以下节选：

```json
{
  "id": "async_example",
  "async_task_id": "async_example",
  "status": "queued",
  "endpoint": "/v1/images/generations",
  "modality": "image",
  "model": "your-configured-image-model",
  "business_id": "order-0001",
  "external_user_id": "customer-8",
  "external_task_id": "b-task-100",
  "tag_level_1": "subsystem-b",
  "tag_level_2": "product-image",
  "retention_seconds": 604800,
  "quota": 0,
  "billing_state": ""
}
```

202 表示输入已保存并成功入队，不代表供应商生成成功，也不承诺入队时已扣费。初始 `quota:0` 不代表该任务免费。收到 ID 后立即保存到自己的数据库，查询、下载和消费关联都使用此 ID，不使用隐藏的供应商任务 ID。

### 保存期

省略 `retention_seconds` 或传 JSON `null`，新任务采用 604800 秒，即 7 天。显式指定必须为 **600 至 2592000 的整数秒数**：600 为 10 分钟，86400 为 1 天，1209600 为 14 天，2592000 为 30 天。不支持无限保存、小数、0 或小于 600。

完成后的 `completed_at`、`result_expires_at` 都是 Unix 秒。截止时间按生成完成计算；轮询、下载或存储重试不会续期。短保存期不是生成超时。已有旧版任务继续使用原期限；用同一个幂等键不能修改其保存期。

如果需要长期使用，应在 `result_expires_at` 前下载至业务自己的长期存储，并保存自己的资产地址。R2 桶生命周期不能替代接口的有效期检查；由网关部署方确保桶的清理兜底规则不会提前删除允许保留 30 天的对象。

### 幂等与网络超时

每个业务生成尝试先创建并保存固定的 `Idempotency-Key`，最多 128 字节，不包含 CR、LF、NUL。`business_id`、`external_task_id` 本身不提供幂等保护；省略幂等键时每次 POST 都可能创建新任务。

在同一账户和标签域内，相同键及相同请求返回同一任务；改模型、输入、业务归因或归一化后的保存期会返回 409 `idempotency_conflict`。文件重试时还要保持文件名、类型、顺序等元数据一致，multipart 的随机边界本身不影响幂等。

POST 网络超时或网关 5xx 时，不能判断入队是否已经成功。恢复时使用**原键和原请求**重新 POST 以取得同一 ID，不要换键重新生成。如果已取得 ID，优先查询该 ID。任务记录不是永久业务防重数据库，业务端仍需持久化自己的唯一任务及映射。

## 查询状态与下载结果

```bash
curl -sS 'https://gateway.example.com/v1/async/tasks/async_example' \
  -H 'Authorization: Bearer <generation-key>'
```

查询成功为 HTTP 200 的任务对象，与提交形状相同；任务 `status` 不等于 HTTP 状态。建议逐步退避到每 2 至 5 秒查询一次，不要使用高频循环。

| 状态 | 业务端处理 |
| --- | --- |
| `queued` | 等待 Worker 领取 |
| `submitting` | 正在调用供应商，不再提交新生成 |
| `polling` | 供应商已接受任务，网关继续查询原任务 |
| `storage_pending` | 正在保存结果，存储重试不会重新生成 |
| `completed` | 请求结果并及时下载或转存 |
| `failed` | 展示 `error_code`、`error_message`，由业务决定下一次尝试 |
| `unknown` | 结果或提交状态不确定，继续核对或人工对账，禁止盲目重新生成 |

业务端等待超时不是任务取消。中断轮询后可以继续查询同一个 ID。一个已失败任务也不会因同键重放自动重新执行；新的业务尝试应先明确确认重试决策，再使用新的尝试 ID 和幂等键。

完成后读取结构化结果：

```bash
curl -sS 'https://gateway.example.com/v1/async/tasks/async_example/result' \
  -H 'Authorization: Bearer <generation-key>'
```

图片结果保留供应商 JSON 的其他字段，图片的 `b64_json` 被归档为文件，并替换成受鉴权的相对地址。典型结果节选：

```json
{
  "data": [
    {"url":"/v1/async/tasks/async_example/artifacts/image-1"}
  ]
}
```

原生异步视频等结果使用 `{id,model,status,data:[{id,type,url}]}`。二进制语音、SRT 或普通文本输出也归一为这种结构，条目额外包含 `mime_type` 和 `size`；JSON 转录可直接返回供应商的 `{text:...}` 等对象，不保证包含 `data`。不要假设所有结果都有 `artifacts[]`、`asset_ref`、固定 MIME 或固定文件名。

使用结果返回的地址下载，而不是猜测 `image-1` 或 `output`：

```bash
curl -sS --fail-with-body \
  'https://gateway.example.com/v1/async/tasks/async_example/artifacts/image-1' \
  -H 'Authorization: Bearer <generation-key>' \
  --output './generated-image.png'
```

文件接口返回原始字节及 `Content-Type`，不是 JSON，也不是公开 R2 直链。地址不能直接放入无鉴权的 `<img src>`；应由业务后端下载后转存，或提供验证业务用户身份的文件代理。仅向受信任网关域名发送密钥，不把带密钥请求重定向到其他域名。

账户管理端还可以用 `nap_...` 和 `usage:read` 读取 `/api/business/tasks/<id>`、`/result`、`/artifacts/<artifact_id>`，覆盖本账户全部标签域。管理端取得的结果 JSON 中仍可能是 `/v1/async/tasks/...` 的地址；使用管理令牌下载时应构造对应 `/api/business/tasks/<id>/artifacts/<artifact_id>`，不要拿 `nap_...` 调用 `/v1` 文件接口。

## 其他媒体操作

| `endpoint` | 请求方式 | 说明 |
| --- | --- | --- |
| `/v1/images/generations` | JSON 外层封装 | 图片生成 |
| `/v1/images/edits` | multipart | 图片编辑，上传实际图片字节 |
| `/v1/videos` | JSON 或适配器要求的 multipart | 视频生成，具体参数由渠道协议决定 |
| `/v1/audio/speech` | JSON 外层封装 | 非流式语音生成 |
| `/v1/audio/transcriptions` | multipart | 语音转录，上传实际音频 |
| `/v1/audio/translations` | multipart | 音频翻译，上传实际音频 |

渠道是否支持该模型及操作仍需单独确认。文本对话继续使用原同步或 SSE 接口，不放进这个异步接口。新包装层拒绝 `stream:true` 和 `stream_format:"sse"`，也不接受任意 URL 代理或任意插件任务路由。

图片编辑示例，元数据和供应商字段都直接是 form 字段，不再套 `request`：

```bash
curl -sS -X POST 'https://gateway.example.com/v1/async/tasks' \
  -H 'Authorization: Bearer <generation-key>' \
  -H 'Idempotency-Key: b-edit-0001-attempt-1' \
  -F 'endpoint=/v1/images/edits' \
  -F 'retention_seconds=600' \
  -F 'business_id=edit-0001' \
  -F 'external_task_id=b-edit-100' \
  -F 'model=your-configured-image-model' \
  -F 'prompt=Replace the background with a beach' \
  -F 'n=1' \
  -F 'image=@./input.png;type=image/png'
```

音频转录示例需要允许 `audio` 的生成密钥，不要复用前面只允许图片的密钥：

```bash
curl -sS -X POST 'https://gateway.example.com/v1/async/tasks' \
  -H 'Authorization: Bearer <audio-generation-key>' \
  -H 'Idempotency-Key: b-transcription-0001-attempt-1' \
  -F 'endpoint=/v1/audio/transcriptions' \
  -F 'model=your-configured-transcription-model' \
  -F 'retention_seconds=86400' \
  -F 'external_user_id=customer-8' \
  -F 'external_task_id=b-audio-100' \
  -F 'file=@./sample.mp3;type=audio/mpeg'
```

文件输入须为实际上传字节，或原适配器支持的内联 Base64/data 格式。新异步层拒绝媒体 URL 和供应商 `file_id` 等外部文件标识，不提供 `asset_ref` 上传 API；输入不能依赖排队时可能失效的远程链接。普通提示词内的 URL 仍是文字，不表示上传媒体。

完整提交体含 multipart 开销上限为 64 MiB；捕获响应和单个归档文件也各有 64 MiB 限制。Base64 会放大体积，不要以原文件大小直接判断提交体是否超限。大视频超限应另选已支持的原生接口或改造方案，不能把远程 URL 放进新包装层来绕过限制。

## Node.js 后端示例

下方示例使用 Node.js 20 及以上的内置 `fetch`，仅用于可信后端。没有自动重新生成：提交异常由调用方用同一幂等键恢复；轮询遇到失败或未知状态会交回业务处理；等待超时保留原任务 ID。

```javascript
function createMediaClient({ baseUrl, generationKey, fetchImpl = fetch }) {
  const origin = new URL(baseUrl);
  if (origin.protocol !== 'https:') throw new Error('HTTPS is required');
  origin.pathname = '/';
  origin.search = '';
  origin.hash = '';

  async function request(path, options = {}, binary = false) {
    const url = new URL(path, origin);
    if (url.origin !== origin.origin) throw new Error('Untrusted gateway origin');
    const response = await fetchImpl(url, {
      ...options,
      headers: { ...options.headers, Authorization: `Bearer ${generationKey}` },
      redirect: 'error',
      signal: AbortSignal.timeout(30000),
    });
    if (binary && response.ok) return response;
    const body = await response.json().catch(() => null);
    if (!response.ok || body?.success === false) {
      const error = new Error(body?.error?.message || body?.message || 'Gateway request failed');
      error.httpStatus = response.status;
      error.code = body?.error?.code || body?.code;
      throw error;
    }
    if (body === null) throw new Error('Expected a JSON response');
    return body;
  }

  const client = {
    submit(payload, idempotencyKey) {
      if (!idempotencyKey) throw new Error('Persist an idempotency key before submitting');
      return request('/v1/async/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
        body: JSON.stringify(payload),
      });
    },
    getTask(id) {
      return request(`/v1/async/tasks/${encodeURIComponent(id)}`);
    },
    getResult(id) {
      return request(`/v1/async/tasks/${encodeURIComponent(id)}/result`);
    },
    download(id, artifactUrl) {
      const url = new URL(artifactUrl, origin);
      const prefix = `/v1/async/tasks/${encodeURIComponent(id)}/artifacts/`;
      const artifactId = url.pathname.slice(prefix.length);
      if (url.origin !== origin.origin || !url.pathname.startsWith(prefix) ||
          !artifactId || artifactId.includes('/') || url.search || url.hash) {
        throw new Error('Expected an artifact URL for this gateway task');
      }
      return request(url.pathname, {}, true);
    },
    async wait(id, maxWaitMs = 15 * 60 * 1000) {
      const deadline = Date.now() + maxWaitMs;
      let delay = 2000;
      while (Date.now() < deadline) {
        let task;
        try {
          task = await client.getTask(id);
        } catch (error) {
          if (![429, 502, 503, 504].includes(error.httpStatus)) throw error;
        }
        if (task?.status === 'completed') return task;
        if (task?.status === 'failed' || task?.status === 'unknown') {
          const error = new Error(task.error_message || `Task is ${task.status}`);
          error.task = task;
          throw error;
        }
        const remaining = deadline - Date.now();
        if (remaining <= 0) break;
        await new Promise(resolve => setTimeout(resolve, Math.min(delay, remaining)));
        delay = Math.min(Math.round(delay * 1.5), 5000);
      }
      const error = new Error('Waiting stopped; resume polling the same task ID');
      error.taskId = id;
      throw error;
    },
  };
  return client;
}
```

提交端使用 `client.submit(payload, persistedIdempotencyKey)` 后保存返回的 `id` 并立即向业务前端响应。业务后台任务再使用 `client.wait(id)` 和 `client.getResult(id)`；网络异常时调度稍后重查，不把异常当成新生成的理由。

对文件型结果遍历 `result.data` 内有 `url` 的条目，调用 `client.download(id, item.url)`，将返回的 `Response.body` 流式传给自己的资产存储。JSON 转录则直接处理 `result.text` 等字段。不要把 `wait` 放在长时间占用的业务 HTTP 请求里，也不要把这个包含密钥的客户端放到浏览器中。

## 消费统计和业务关联

业务管理后端使用 `nap_...` 和 `usage:read` 查询本账户消费日志；不是用 `sk-...` 调管理接口：

```bash
curl -sS --get 'https://gateway.example.com/api/log/self' \
  -H 'Authorization: Bearer <management-access-token>' \
  --data-urlencode 'p=1' \
  --data-urlencode 'page_size=20' \
  --data-urlencode 'type=2' \
  --data-urlencode 'tag_level_1=subsystem-b' \
  --data-urlencode 'tag_level_2=product-image' \
  --data-urlencode 'async_task_id=async_example'
```

成功为 `{success:true,data:{page,page_size,total,items:[...]}}`，`page_size` 最大 100。可按 `business_system_id`、两级标签、`business_id`、`external_user_id`、`external_task_id`、`async_task_id` 筛选，还可传 `model_name`、`token_name`、`request_id`、`upstream_request_id` 以及 Unix 秒的 `start_timestamp`、`end_timestamp`。

未传标签参数表示不筛选该标签；显式传 `tag_level_1=` 只匹配空一级标签，不是所有域。`type:2` 为消费、`type:5` 为失败、`type:6` 为退款，查询全部类型可省略 `type`。消费日志的 `quota`、`model_name`、业务关联字段位于记录顶层；`other` 是 JSON 字符串，不是嵌套对象。界面的日志 `id` 为展示序号，不适合做跨次查询的永久去重键。

可通过 `GET /api/log/self/stat` 使用相同业务筛选获取 `data.quota`、`rpm`、`tpm`，但这不是任务成功次数或业务用户数统计接口。额度换算遵循当前站点配置；不要把每条额度直接当金额。日志可能因持久化 outbox 重试稍晚出现，不要把暂时查不到日志当作免费或允许重复生成。

业务自己的数据库建议保存业务任务 ID、网关任务 ID、业务用户、子系统和产品、请求尝试号、最终任务状态、额度及已转存资产位置。自己统计生成次数和用户维度，使用网关任务 ID 对账；任务状态、消费记录和业务收费不是同一个概念。

原同步或 SSE 请求也可传 `X-Business-ID`、`X-External-User-ID`、`X-External-Task-ID` 请求头记录归因，标签仍由生成密钥决定。这样不等于启用异步或 R2 二次归档。

## 错误处理与上线检查

异步控制器典型错误为 `{error:{code,message,type}}`；鉴权等中间件也可能返回 `{success:false,message}`。不要只解析一种格式，或只按 HTTP 200 判断任务成功。

| HTTP 状态和错误 | 处理 |
| --- | --- |
| 400 `invalid_retention_seconds` | 保存期须为 600 至 2592000 的整数秒数 |
| 400 `invalid_async_request` | 检查 JSON 或 multipart 格式；分数、重复 TTL 字段等可能在解析阶段失败 |
| 400 `external_async_input_not_supported` | 改为实际文件字节或已支持的内联格式，不传远程媒体 URL 或供应商文件 ID |
| 401 / 403 | 核对生成密钥与管理令牌类别、scope、状态、模型和模态权限，不能靠重试绕过 |
| 404 `task_not_found` | ID 不存在、访问域不符或摘要已过期，跨域不会暴露任务是否存在 |
| 404 `artifact_not_found` | 使用结果中实际返回的文件地址，不猜文件 ID |
| 409 `idempotency_conflict` | 同键请求参数发生变化，查原尝试，不换键自动重发 |
| 409 `result_not_ready` | 先查询任务状态；失败或未知任务也不会有可读取的完成结果 |
| 410 `result_expired` | 结果有效期结束，不能靠换密钥或重复下载续期 |
| 413 `request_too_large` | 完整请求体超过 64 MiB，包含 multipart 或 Base64 开销 |
| 429 `async_pending_limit` | 本账户排队及活跃任务过多，稍后用原幂等键提交 |
| 503 `async_storage_not_configured` | 由部署方补全私有 R2 配置，不能回退本地目录 |
| 503 `unsupported_billing_configuration` / `unsupported_log_database` | 由部署方调整缓存、批量计费或日志数据库配置 |
| 503 `request_storage_failed` / `async_admission_failed` | 稍后用原键及原请求恢复提交，不创建新的业务尝试 |
| 503 `result_unavailable` | 暂时无法读取已保存结果，重试读取，不重新生成 |

任务摘要通常保留完成后 30 天，结果有效期结束时 `/result` 和文件接口立即拒绝访问；到摘要期限后，任务查询也可能返回 404。结果无法永久依靠网关查询，应以业务自己的任务和资产记录为准。

上线前至少用测试账户验证以下行为：

- 相同请求和幂等键多次提交只得到同一任务；模拟 POST 响应丢失后仍能找回原 ID。
- 不同账户、不同标签不能读取结果；轮换为同账户同标签密钥可以读取；业务用户只能查看自己的任务。
- 图片、音频和实际使用的视频渠道逐一验证提交、完成、下载，确认解析了对应结果格式。
- 600 秒有效期在完成后开始，超时下载为 410；业务资产已在截止前转存。
- 禁用密钥后不再允许其发起生成或读取；业务重试不会对 `unknown` 自动重新生成。
- 对齐实际消费日志与业务订单，并确认后台轮询重启后能从数据库的任务映射继续运行。

本阶段没有 webhook、任务取消、包装层任务列表、任意插件路由包装、永久 R2 直链或业务级跨模型兜底。New API 仍部署在 Go/Docker 服务中，不会因为使用 R2 或 Supabase 而变成免费计算；原点墨任务、重试、兜底和同步调用方式保持不变。
