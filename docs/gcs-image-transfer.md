# 生图结果转存自建 CDN（GCS）

> 状态：第 1 期已实现（`/v1/images/generations`、`/v1/images/edits`）
> 设计文档：`docs/superpowers/specs/2026-08-06-image-gen-cdn-design.md`
> 视频任务转存见 `docs/gcs-video-transfer.md`（同一个 bucket，不同前缀）

生图结果原本原样透传上游：要么是**有时效性的上游直链**（几小时到几天过期、上游不可控），
要么是**数 MB 的 base64**（响应体巨大、无法留存追溯）。开启本功能后，网关会把上游图
转存到自己的 GCS bucket，对外统一返回我们自己的 V4 签名链接，同时保留 `b64_json` 能力。

**核心约定：转存失败一律回退透传上游原始结果。** 用户永远不会因为 GCS 故障拿不到图。
这不只是体验取舍——返回 relay error 会触发渠道重试、最终失败还会退款，是计费正确性红线。

## 1. 配置

| 环境变量 | 默认值 | 说明 |
|---------|-------|------|
| `GCS_IMAGE_TRANSFER_ENABLED` | `false` | **生图转存总开关**，独立于视频侧 `GCS_TRANSFER_ENABLED` |
| `GCS_READ_ONLY_ENABLED` | `false` | 两个写开关都关闭时仍初始化 GCS client，以便读取历史 `gs://` 结果（见 6） |
| `GCS_IMAGE_PREFIX` | `api/image` | 对象前缀，与视频 `api/video` 同 bucket 不同前缀 |
| `GCS_IMAGE_SIGNED_URL_TTL` | `168h`（7 天） | 签名链接有效期。**解析时钳制到 V4 协议上限 7 天**，超限值 GCS 会直接拒签 |
| `GCS_IMAGE_MAX_SIZE` | `32MiB` | 单图体积上限，超限判转存失败并回退 |
| `GCS_IMAGE_TRANSFER_TIMEOUT` | `60s` | 同步 HTTP 路径的单图转存预算（经 context 强制） |
| `GCS_IMAGE_STREAM_TIMEOUT` | `5s` | 流式路径预算（第 2 期用；必须严格小于 stream scanner 的 10s ping 等待） |
| `GCS_IMAGE_MJ_TIMEOUT` | `30s` | Midjourney 后台转存预算（第 3 期用） |
| `GCS_IMAGE_CONCURRENCY` | `4` | 并发转存数上限 |
| `GCS_IMAGE_CAPTURE_MAX` | `64MiB` | 响应缓冲上限，超过即放弃改写、原样直通 |
| `GCS_IMAGE_STRIP_B64_WHEN_URL` | `false` | 未传 `response_format` 且转存成功时删除 `b64_json`（响应瘦身，**属行为变更**） |
| `GCS_IMAGE_DROP_ALI_METADATA` | `true` | **仅 Ali 渠道**：转存成功时删除顶层 `metadata`（Ali 把完整上游响应写进该字段，内含上游直链） |
| `GCS_SIGN_CACHE_MAX_ENTRIES` | `10000` | 签名缓存条目上限 + 惰性清扫阈值，与既有 `GCS_SIGN_CACHE_TTL` 配合。**生图不入签名缓存**（一次性对象名只访问一次，入缓存等于内存泄漏），该上限保护的是视频与 MJ 的缓存分区 |

bucket / 保留期 / 凭证沿用视频侧配置：`GCS_RESULT_BUCKET`、`GCS_RESULT_RETENTION_DAYS`、
`GOOGLE_APPLICATION_CREDENTIALS`。

关闭 `GCS_IMAGE_TRANSFER_ENABLED` 时：不安装响应缓冲 writer，全部走原逻辑，
**零开销、零行为变化**。

## 2. `response_format` 适配

| 客户端传入 | 返回 |
|-----------|------|
| `url` | 只填 `url` = 签名链接；**删除** `b64_json` 键 |
| `b64_json` | 只填 `b64_json` = base64（**仍然上传 GCS 留存**）；**删除** `url` 键 |
| 未传 | 按上游形态适配，且**永远补上 `url`**：上游给直链 → `url` 替换为签名链接；上游给 base64 → `url` = 签名链接 **且** `b64_json` 原样保留 |

「未传」这条的取舍：现有依赖 `b64_json` 的客户端（gpt-image、Gemini、Zhipu 4V）
一个字段都不会丢，同时所有响应都多出一个可用的 CDN 链接。代价是这类响应体依然很大——
需要瘦身时打开 `GCS_IMAGE_STRIP_B64_WHEN_URL`。

**同一元素同时带 `url` 与 `b64_json` 时**（Ali 可以同时构造）：转存源**优先取 `b64_json`**
（字节已在手里，省一次跨境下载与一次 SSRF 风险面）；解码失败再退回 `url`。

改写是**无损**的：只增删 `url` / `b64_json` 两个键，`revised_prompt`、顶层 `usage`、
供应商扩展字段（如 MiniMax 的 `metadata`）全部原样保留。

## 3. 对外契约

- **签名 URL 有效期 7 天，过期后无法刷新**——`/v1/images/*` 没有任务记录，网关无法二次现签。
  需要长期持有请用 `response_format=b64_json`，或自行把图落库/转存。
- 返回的 URL **不保证稳定也不保证每次不同**，客户端不应做 URL 等值比较。
- 结果保留期由 bucket 生命周期规则决定，必须与 `GCS_RESULT_RETENTION_DAYS` 保持一致；
  签名 TTL 会按剩余保留期收口，不会签出在有效期内就被删除的链接。
- **响应耗时现在包含转存时间**（消费日志里的耗时口径变化，需通告业务方）。

## 4. 不覆盖的入口

| 入口 | 状态 | 原因 |
|------|------|------|
| chat completions 里的内联图片 | 第 2 期 | 现为 `![image](data:image/png;base64,...)` 内联在 message content |
| Responses API `image_generation_call` | 第 2 期 | 需要 raw JSON patch 注入 `result_url` |
| Midjourney | 第 3 期 | 有 DB 记录 + 已有 `/mj/image/{id}` 代理端点，可拿到保留期内永久有效的链接 |
| **Gemini 原生格式**（`/v1beta/...:generateContent`） | **明确排除** | 原生协议的输出 part 没有 URL 字段（`fileData.fileUri` 是输入侧概念），改写会破坏所有按官方 SDK 解析的客户端。该入口的图片仍是内联 base64 |
| `stream=true` 的 SSE 生图（gpt-image partial_images） | 不覆盖 | 流式响应无法缓冲改写，原样透传 |
| `/v1/edits` | 不适用 | 路由声明在 image 组，但 relay mode 是遗留的 `RelayModeEdits`，不会进入 `ImageHelper`，当前不是可工作的生图入口 |

## 5. 行为变更通告

上线前需要告知业务方的三点：

1. `GCS_IMAGE_STRIP_B64_WHEN_URL=true` 会让**未传 `response_format`** 的客户端拿不到
   `b64_json` 字段。默认关闭。
2. `GCS_IMAGE_DROP_ALI_METADATA=true`（**默认开启**）会让 Ali 通义万相的客户端拿不到
   顶层 `metadata`。该字段是非 OpenAI 标准字段，内容是完整上游响应（含上游直链）。
   若有客户端依赖它，关掉开关即可，但会保留上游直链泄露。
3. 消费日志里的**响应耗时包含转存时间**，耗时统计口径变化。

## 6. 故障处置

**所有转存失败都会回退透传上游原始结果**，用户始终能拿到图。因此故障表现为"链接又变回
上游直链"，而不是请求失败。

看日志里的 `gcs-metrics image` 行区分原因：

| 计数器 | 含义 | 处置 |
|-------|------|------|
| `download_fail` | 上游取流失败（含非 200） | 上游 CDN 提前过期或上游不稳定，看具体渠道 |
| `gcs_service_fail` | GCS 5xx / 网络 / 超时 | GCS 侧故障，必要时用总开关止血 |
| `gcs_auth_fail` | GCS 鉴权失败（401/403/凭证失效） | 检查 SA 凭证与 bucket 权限 |
| `sign_fail` | V4 签名失败 | Workload Identity 路径检查 `roles/iam.serviceAccountTokenCreator` |
| `oversize` | 超过 `GCS_IMAGE_MAX_SIZE` | 按需调大上限 |
| `mime_reject` / `decode_fail` | 上游返回的不是白名单图片类型 / base64 损坏 | 看具体渠道的上游响应 |
| `corrupt_object` | 复用已存在对象前的完整性校验不通过 | 需告警，可能有半截对象 |
| `passthrough` | 整体未改写（响应体不可识别 / 非 JSON / 缓冲超限 / 全部转存失败） | 结合上面各项定位 |

单条错误日志形如：

```
gcs-image transfer fail kind=download-fail index=0 err=gcs image transfer: upstream returned 403: ...
```

**止血开关**：`GCS_IMAGE_TRANSFER_ENABLED=false` + 重启，立即恢复原样透传。

## 7. 只读模式（重要）

`GCS_IMAGE_TRANSFER_ENABLED` 与 `GCS_TRANSFER_ENABLED` 都关闭时，GCS client 默认**不初始化**。
如果之前已经有结果被存成 `gs://`（尤其第 3 期的 Midjourney），关掉写开关后仍必须能签名读出，
否则那些存量结果会变成裸 `gs://` 泄露给用户。

这种情况必须显式设置：

```
GCS_READ_ONLY_ENABLED=true
```

三个就绪判断相互独立，不会互相牵连：

- `GCSClientReady()` —— client 可用（读取/签名的前置条件），与写开关无关
- `GCSVideoTransferReady()` —— 视频 worker 的启动条件
- `GCSImageTransferReady()` —— 生图写入侧的启动条件

## 8. 上线前检查

1. **SA 权限**：`roles/storage.objectCreator` + `roles/storage.objectViewer`
   （或自定义角色含 `storage.objects.create` + `storage.objects.get`）。
   **objectCreator 单独不够**——签名 GET URL 在服务端以签名 SA 的身份鉴权，
   SA 自身必须持有 `storage.objects.get`，否则签出的链接全部 403。不要用 objectAdmin。
2. **实签验证**：用目标 SA 实签一个 GET URL 并 `curl` 验证返回 200，再放量。
   进程启动时也会做一次签名自检，失败直接 fatal 退出（不静默带病启动）。
3. **凭证部署到所有实例**：签名发生在每个副本上，不只 master。
4. **bucket 生命周期规则**必须与 `GCS_RESULT_RETENTION_DAYS` 一致。
5. **NTP**：V4 签名对本地时钟敏感（偏差 >15 分钟会被 GCS 拒绝）。
6. **压测**：生图 QPS 通常远高于视频。单请求内存峰值可达图片大小的 3-5 倍
   （上游响应本身已 ReadAll + 缓冲副本 + raw JSON map + 解码字节 + tee 缓冲），
   放量前先按 `GCS_IMAGE_MAX_SIZE` / `GCS_IMAGE_CAPTURE_MAX` 与实例内存核算并做大图并发压测。
7. **费用**：每张图经网关下载 + 上传各一次，另有 GCS 存储与出口流量费用。
   先看 `gcs-metrics image` 的 `bytes_downloaded` / `bytes_uploaded` 再放量。
