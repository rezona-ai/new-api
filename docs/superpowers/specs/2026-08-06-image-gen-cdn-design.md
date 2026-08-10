# 生图结果转存自建 CDN（GCS）设计文档

> 状态：**方案定稿，可进入实现**（已过三轮 codex 只读核验，18 条阻断问题全部人工复核后修订）
> 日期：2026-08-06
> 前置阅读：`gcs-video-transfer-design.md`（视频任务转存，已实现）、`docs/gcs-video-transfer.md`
> 评审记录见第 7 节

## 1. 背景与目标

视频任务结果已经统一转存 GCS 并以 V4 签名链接返回（`service/gcs_storage.go` / `gcs_transfer.go` / `task_result_url.go`）。
生图链路仍然原样透传上游结果：要么是**有时效性的上游直链**（几小时到几天过期、上游不可控），要么是**数 MB 的 base64**（响应体巨大、无法留存追溯）。

**目标**：生图结果也走一遍自建 CDN（同一个 GCS bucket），对外统一返回我们自己的链接；同时**保留 base64 返回能力**，不破坏现有客户端。

### 与视频转存的关键差异（决定了本设计的骨架）

| 维度 | 视频任务 | 生图 |
|------|---------|------|
| 时序 | 异步任务（提交 → 轮询 → 终态） | **同步请求**：上游响应即用户响应 |
| 转存时机 | 轮询发现上游成功后，异步 worker 转存 | **必须在响应下发前同步完成** |
| 状态机 | `model.Task` + CAS 单赢家 + 退款兜底 | `/v1/images/*` 与 Responses **无任务记录、无状态机**；MJ 有记录 |
| 失败代价 | 可退款、可重试 | 上游已产图、额度已预扣，**不能因转存失败让用户拿不到图** |
| 重签能力 | 有 DB 记录 → 读时现签、URL 保留期内永久有效 | `/v1/images/*` 与 Responses 签一次、TTL 内有效；MJ 走代理端点可读时现签 |

推论：生图转存**不引入任何状态机、不引入退款路径、不返回 relay error**。失败一律回退透传上游原始结果（4.9）。

## 2. 现状调研

### 2.1 生图的入口全景

| # | 入口 | 代码路径 | 结果形态 | 本设计 |
|---|------|---------|---------|--------|
| 1 | `/v1/images/generations`、`/v1/images/edits` | `relay/image_handler.go:114` → 各渠道 `DoResponse` | `data[].url` / `data[].b64_json`；OpenAI 兼容渠道为上游原始 body 透传 | **第 1 期** |
| 2 | chat completions 里的内联图片 | `relay/channel/gemini/relay-gemini.go:1106`（非流式）、`:1265`（流式） | `![image](data:image/png;base64,...)` 塞进 message content | **第 2 期** |
| 3 | Responses API `image_generation_call` | `relay/channel/openai/relay_responses.go:20`（非流式）、`:82-91`（流式） | `output[].result` = base64 | **第 2 期** |
| 4 | Midjourney | `controller/midjourney.go:140-141`（轮询）、`relay/mjproxy_handler.go:599-618`（`code=21` 直接成功） | `midjourney.ImageUrl` 存上游直链 | **第 3 期** |
| 5 | **Gemini 原生格式**（`/v1beta/...:generateContent`） | 路由 `router/relay-router.go:197`（`:189` 只是 route group）；`relay/channel/gemini/relay-gemini-native.go:20`（非流式）、`:81`（流式）**原样透传** | 原生 `inlineData` base64 | **明确排除**，见 3.2 |
| 6 | `/v1/images/variations` | `router/relay-router.go:154` → `RelayNotImplemented` | — | 不适用 |
| 7 | `/v1/edits` | 路由声明为 image 组（`router/relay-router.go:109`），但 relay mode 是遗留的 `RelayModeEdits`（`relay/constant/relay_mode.go:73`、最终落到 `relay/compatible_handler.go:28`），而 `relayHandler` 只把 `RelayModeImagesGenerations`/`RelayModeImagesEdits` 分发给 `ImageHelper`（`controller/relay.go:38`） | — | **不是可工作的生图入口**，本期不动、不修路由 |
| 8 | MJ notify 回调 `RelayMidjourneyNotify`（`relay/mjproxy_handler.go:111` 写裸 `ImageUrl`、`:123` `Update()`） | 路由已注释（`router/relay-router.go:217`） | — | 非活动路径；**恢复该路由时必须接入转存**（写进代码注释） |

### 2.2 入口 1：各渠道「不传 `response_format`」时的实际返回（逐条核对代码）

| 渠道 | 不传 `response_format` 时返回 | 依据 |
|------|------|------|
| OpenAI 兼容 | **原样透传上游 body** → dall-e-3 给 `url`；gpt-image-1 只给 `b64_json` | `relay/channel/openai/relay-openai.go:555-570` |
| XAI / Volcengine / BaiduV2 / SiliconFlow | 复用上面同一条透传路径 | `xai/adaptor.go:114`、`volcengine/adaptor.go:391`、`baidu_v2/adaptor.go:118`、`siliconflow/adaptor.go:113` |
| Ali 通义万相 | `url`=上游直链；`b64_json` 仅当上游自带 `b64_image`。**另把完整上游响应写进顶层 `metadata`** | `ali/dto.go:136-156`、`ali/image.go:268,279` |
| MiniMax | url。**也会输出顶层 `metadata`（上游 metadata 直传，非调试字段）** | `minimax/image.go:145-172` |
| Jimeng | url（`ReturnURL=true`） | `jimeng/adaptor.go:77-79` |
| Replicate | url（仅显式 `b64_json` 才转 base64） | `replicate/adaptor.go:250,257-274` |
| Zhipu 4V | **优先用上游 base64**（`b64_json`/`b64_image`），缺失时才由网关下载 URL 转 base64；输出结构只有 `b64_json` | `zhipu_4v/image.go:93-124` |
| Gemini（含 Vertex Imagen，最终委托 Gemini handler `vertex/adaptor.go:353-355`） | **恒定 b64_json**（上游只给 `BytesBase64Encoded`） | `relay/channel/gemini/relay-gemini.go:1646-1652` |

结论：现状不存在统一的「默认形态」。默认行为必须**按上游实际形态适配**（决策 3）。

### 2.3 可复用与需改造的既有设施

**可直接复用**
- `service/gcs_storage.go:131` `GCSUploadObject`：`If-GenerationMatch:0` 条件写 + 错误路径 cancel context 放弃上传（绝不 finalize 半截对象）
- `service/gcs_storage.go:170` `GCSVerifyExistingObject`：复用已存在对象前的 size/CRC32C 校验
- `service/gcs_transfer.go:80` `gcsCountingReader`：`LimitReader(N+1)` + 字节计数 + CRC32C 累计（裸 `LimitReader` 在 N 字节静默 EOF，会把超限文件截断成"成功"对象）——包内私有，新文件同在 `service` 包
- `service/gcs_metrics.go`：指标上报骨架

**需要改造后才能用**
- `service/gcs_storage.go:205-250` `GCSSignResultURL`：TTL 钉死全局 `GCSSignedURLTTL`（默认 12h，`setting/gcs_setting.go:51`），签名缓存**只以 objectName 为 key**（`:224`）且只校验缓存年龄——图片 7 天 TTL 会与视频 12h 互相污染。见 4.4
- `service/gcs_storage.go:59` GCS client 初始化只看视频开关；视频 worker 又用 `GCSStorageReady()` 作为启动条件（`gcs_transfer.go:156,229`）——不能简单改成"两开关取或"。见 4.5
- `service/gcs_transfer.go:49-78` `gcsObjectContentType`：白名单（`:49`）已含 webp 但缺 gif，扩展名映射（`:60-78`）同时缺 webp/gif，整体面向视频资产。图片单独实现
- `service/download.go:52` `DoDownloadRequest`：非 Worker 分支用 `http.Client.Get`、Worker 分支用 `http.Client.Post`（`:49`），**两条都无 context**。需新增 context 版本（两条分支都要绑）

## 3. 已确认的决策

| # | 决策 | 说明 |
|---|------|------|
| 1 | 覆盖入口 1/2/3/4，分三期实现 | 三期机制完全不同、独立交付 |
| 2 | 对外 URL 形态 = GCS V4 签名 URL | 复用签名设施，零新增基础设施。`/v1/images/*` 与 Responses 无记录不能重签 → TTL 取 V4 上限 7 天；MJ 走已有代理端点 → 读时现签、保留期内有效 |
| 3 | 两种格式都先转存；`response_format` 按 3.1 适配 | |
| 4 | 转存失败 → 回退透传上游原始结果 | 打 error 日志 + 计失败指标；**绝不返回 relay error**（会触发渠道重试，最终失败还会退款） |
| 5 | 生图不引入状态机/退款 | |
| 6 | 改写响应体必须**无损**：只替换目标字段，保留全部未知字段 | 见 4.3 |
| 7 | Gemini 原生格式入口明确排除 | 见 3.2 |
| 8 | **写客户端失败不得转成 relay error** | 见 4.2.3 |

### 3.1 `response_format` 适配规则

| 客户端传入 | 返回 |
|-----------|------|
| `url` | 只填 `url` = 签名链接；**删除** `b64_json` 键 |
| `b64_json` | 只填 `b64_json` = base64（**仍然上传 GCS 留存**）；**删除** `url` 键 |
| 未传 | **按上游形态适配，且永远补上 `url`**：上游给直链 → `url` 替换为签名链接；上游给 base64 → `url` = 签名链接 **且** `b64_json` 原样保留 |

「未传」这条的取舍：现有依赖 `b64_json` 的客户端（gpt-image / Gemini / Zhipu）一个字段都不丢，同时所有响应都多出一个可用的 CDN 链接。代价是这类响应体依然很大——提供 `GCS_IMAGE_STRIP_B64_WHEN_URL`（默认 `false`）开关，需要瘦身时打开即变为纯 url。

「删除键」必须是**真删除 JSON 键**，不是写空字符串——`dto.ImageData` 的 `url`/`b64_json` 没有 `omitempty`（`dto/openai_image.go:180-184`），走 DTO 重新 marshal 会产出 `"url":""`。这是 4.3 必须用 raw JSON 改写的原因之一。

**同一 data 元素同时带 `url` 与 `b64_json` 时**（Ali 可以同时构造，`ali/dto.go:136-156`）：转存源**优先取 `b64_json`**（字节已在手里，省一次跨境下载与一次 SSRF 风险面）；`b64_json` 解码失败再退回 `url`；两者都失败则该元素完全不动。

### 3.2 明确排除：Gemini 原生格式（入口 5）

原生 Gemini 的响应 part 里图片是 `inlineData{mimeType, data}`，**协议没有输出侧 URL 字段**（`fileData.fileUri` 是输入侧概念）。改写成 `fileData` 会破坏所有按官方 SDK 解析的客户端。

因此本期**不改**该入口（`relay-gemini-native.go:20,81` 保持原样透传）。这意味着「所有生图结果都进自有存储」这个目标**在原生 Gemini 入口上不成立**，需写进对外说明。若后续必须覆盖，方案是 `inlineData` → `fileData(fileUri=签名URL)` 并承担客户端兼容风险，单独立项。

## 4. 方案设计

### 4.1 共用原语：`service/gcs_image.go`（新增）

无状态、不碰 DB：

```go
// ImageSource 一张待转存的图片：base64 优先，URL 兜底（见 3.1 末尾）。
type ImageSource struct {
    B64      string // 上游 base64（可带 data: 前缀，内部剥离）
    URL      string // 上游直链
    MimeHint string // 上游 MIME 提示（可空）
}

type ImageTransferResult struct {
    ObjectName string
    SignedURL  string
    ExpiresAt  int64  // Unix 秒，真实签名过期时刻，不得虚标
    Raw        []byte // 仅当 wantBytes=true 时非空（供 b64_json 回传）
}

// ObjectNamer 由调用方按入口语义提供（命名规则差异见 4.2.4 / 4.6 / 4.7）。
type ObjectNamer func(index int, ext string) string

type TransferOpts struct {
    WantBytes     bool          // 需要回传 base64
    ReuseExisting bool          // 固定对象名入口：已存在视为幂等命中（须过完整性校验，见下）
    Timeout       time.Duration // 单张预算（调用方按路径给不同值）
    Sign          bool          // false = 只上传不签名（MJ 写入阶段），SignedURL/ExpiresAt 留空
    SignPolicy    SignPolicy    // Sign=true 时使用；CacheTag 决定是否入签名缓存（4.4）
}

func TransferImage(ctx context.Context, namer ObjectNamer, index int, src ImageSource, opts TransferOpts) (*ImageTransferResult, error)

// TransferImages 并发转存一组图片，逐张独立成败；失败位置返回 nil。
func TransferImages(ctx context.Context, namer ObjectNamer, srcs []ImageSource, opts TransferOpts) []*ImageTransferResult
```

实现要点：

- **MIME 与扩展名**：图片专用映射（`image/png`→`png`、`image/jpeg`→`jpg`、`image/webp`→`webp`、`image/gif`→`gif`，其余判失败不上传），来源顺序 上游 `Content-Type` → `MimeHint` → `http.DetectContentType`(前 512 字节)。**不复用** `gcsObjectContentType`（面向视频资产）。禁止把上游 URL 的路径/查询串拼进对象名。
- **取流**：新增 `service.DoDownloadRequestWithContext(ctx, url, reason...)`——**Worker 分支与非 Worker 分支都要绑 context**（现状 `download.go:49` 的 Worker POST 与 `:68` 的 Get 都无 context）。SSRF 校验沿用 `ValidateURLWithFetchSetting`。原 `DoDownloadRequest` 保留为 `context.Background()` 包装，既有调用方零改动。
- **体积上限**：复用 `gcsCountingReader`，上限 `GCS_IMAGE_MAX_SIZE` 默认 32 MiB。超限 → 上传经 cancel context 放弃、返回失败 → 回退透传。
- **上传**：`GCSUploadObject`（条件写，错误路径不 finalize）。**`ReuseExisting` 语义**：
  - `false`（`/v1/images/*`）：对象名自带随机 token（4.2.4），冲突不可能发生；万一 `ErrGCSObjectExists` 直接判失败回退——**不做随机后缀重试**，因为 412 可能在 `Close()` 才返回（`gcs_storage.go:150-163`），此时 reader 已被消费，无法重放
  - `true`（MJ / Responses：固定对象名，可能多实例并发或事件重复）：`ErrGCSObjectExists` 视为幂等命中，但**必须拿到精确 size/CRC32C 后再校验**。412 可能在 `io.Copy` 阶段就返回（`gcs_storage.go:150`），此时源流未必读完——规定：**412 后继续在同一体积上限与 timeout 内 drain counting reader 至 EOF**，取得完整字节数与 CRC32C，再调 `GCSVerifyExistingObject(objectName, 字节数, CRC32C)`；drain 失败/超限/超时则**判本次失败回退，禁止复用现存对象**。不存在"退化为只看 Size>0"的分支——那会让半截对象被永久复用（`gcs_storage.go:167` 的既有契约）
- **需要 base64 时**：`io.TeeReader` 同时喂 GCS 与内存缓冲（受同一体积上限约束）。上游本就是 base64 时直接复用已解码字节。
- **签名**：`opts.Sign` 为真时调 `GCSSignObjectURL`（4.4），TTL = `GCS_IMAGE_SIGNED_URL_TTL`（默认 7 天）。MJ 写入阶段 `Sign=false`（只存 `gs://`，读时才签）。
- **超时**：`opts.Timeout` 经 context 强制。同步 HTTP 路径从 `c.Request.Context()` 派生；**后台路径（MJ）必须新建独立 context，禁止复用轮询的局部 ctx**（见 4.7.1）。
- **并发**：同一响应内多图并发，上限 `GCS_IMAGE_CONCURRENCY`（默认 4）。

### 4.2 第 1 期：`/v1/images/generations` + `/v1/images/edits`

各渠道的 `DoResponse` 自己把响应体写进 `c.Writer`（多数经 `service.IOCopyBytesGracefully`，`service/http.go:92`）。因此在 `relay/image_handler.go` 给 `c.Writer` 套一层缓冲 writer，把渠道写出的响应体截下来改写后再真正下发。已核验：`relay/image_handler.go:114` 之后只有数量/日志/计费，没有第二处成功响应正文写出；所有图片渠道都经 `c.Writer`，无人绕过 Gin。

#### 4.2.1 缓冲 writer 契约（`relay/helper/capturing_writer.go` 新增）

实现完整 `gin.ResponseWriter`：`Header`/`Status`/`Size`/`Written`/`WriteHeader`/`WriteHeaderNow`/`Write`/`WriteString`/`Flush`/`Hijack`/`Pusher`/`CloseNotify`，并提供 `Unwrap()` 返回底层 writer。自行维护状态码与已写字节数，不能只靠嵌入原 writer。

**Header 事务隔离（红线）**：安装时 clone 底层 `Header()` 到 shadow header，buffered 阶段所有 header 读写只作用于 shadow。

- 提交（改写成功或原样 flush）时：把 shadow 精确同步回底层（含删除 4.3 要求清理的实体 header）
- 切 passthrough 时：先同步 shadow，再放行后续写入
- **discard（错误路径）时：底层 header 一个字节都不改动**

否则渠道尝试设的 `Content-Type`/`Content-Length`/`ETag`/`Content-Encoding` 会污染后续渠道重试（`controller/relay.go:190`）或统一错误响应（`controller/relay.go:89` 的 `c.JSON`），4.2.3「干净 writer」就不成立。

**两种模式**：

- **buffered（默认）**：`WriteHeader`/`Write`/`WriteString` 全部落缓冲。**`Flush()` 被吞掉**（只做"逻辑 flush"：维护 `Written()=true`、`Size()` 不变）——`IOCopyBytesGracefully` 在 `service/http.go:126` **无条件**调用 `c.Writer.Flush()`，普通非流式 JSON 也会触发；把 Flush 当作"必须立刻透传"会让 OpenAI/Ali/Zhipu 等主流渠道全部绕过改写。流式与否在安装前已判定（`relay/image_handler.go:97-100` 由 Content-Type 设 `info.IsStream`），buffered 下吞 Flush 安全。
- **passthrough**：切换时先按序把已缓冲内容写入底层，此后写入直达底层并标记 `committed=true`（本次响应不可改写）。触发条件：
  1. 响应 Content-Type 为 `text/event-stream` —— **检查点必须覆盖 `WriteHeader`、`WriteHeaderNow` 与首次 `Write`**（渠道可能不显式调 `WriteHeader`）
  2. 缓冲累计超过 `GCS_IMAGE_CAPTURE_MAX`（默认 64 MiB）
  3. `Hijack()` 被调用（WebSocket 等）：**先 commit 再 delegate**

`CloseNotify` 直接代理底层；`Pusher` 返回底层实现。

#### 4.2.2 `ImageHelper` 收口流程

```
转存开启 && !info.IsStream → 安装 capturingWriter（defer 恢复原 writer + 兜底提交/丢弃）
adaptor.DoResponse(...)
  ├─ 返回 error：
  │    committed==false → 丢弃 body 缓冲、底层 header 不动、恢复原 writer、原样返回该 error
  │                      （外层 controller/relay.go:190 的重试循环与 :89 的统一错误响应
  │                       需要一个干净的 writer；先刷渠道错误体会造成重复响应/状态码已提交）
  │    committed==true  → 已经写出，什么都不做，原样返回 error（与今天行为一致）
  └─ 返回成功：
       committed==true / status != 200 / Content-Type 非 JSON → 原样提交，不改写
       否则尝试无损改写（4.3）：
         解析失败 / data 为空 → 原样提交
         成功 → 提交改写后的 body（重设 Content-Length、清理实体 header）
```

> 语义差异（写进 CHANGELOG）：改造后「渠道写了 2xx 正文又返回 error」会变成"丢弃正文 + 干净重试"，今天则是"正文 + 错误体拼接的畸形响应"。仓库现有约定本就禁止这种组合（`relay-openai.go:569-573` 注释：写给客户端之后不应再返回错误），因此这是修正而非退化。

#### 4.2.3 写客户端失败的语义（决策 8）

现状有两个图片渠道在正文写入失败后返回 relay error：Jimeng（`jimeng/image.go:84-86`）、MiniMax（`minimax/image.go:208-210`）。改造后 buffered 模式的 `Write` 只写内存、**不会**暴露真实客户端 I/O 错误，这两条分支实际不再触发；真正的客户端写失败推迟到 `ImageHelper` 内的最终提交点。

规定：**最终提交（写客户端）失败只记 error 日志，仍照常完成结算，不得转成 relay error。** 理由：此时响应可能已部分提交，返回 error 会触发渠道重试与重复响应；这也与 `relay-openai.go:569-573` 的既有原则一致。

#### 4.2.4 对象命名

`{GCS_IMAGE_PREFIX}/{yyyymmdd}/{requestID}_{index}_{rand4}.{ext}`

- `requestID` 取 `info.RequestId`（`relay/common/relay_info.go:145`），便于按请求追溯
- `rand4` 保证跨渠道重试/请求 id 碰撞都不会撞对象名（配合 `ReuseExisting=false`）
- 日期前缀便于人工排查与生命周期观察

### 4.3 无损改写（决策 6）

**禁止**把响应体反序列化成 `dto.ImageResponse` 再重新 marshal —— 会丢顶层 `usage`、供应商扩展字段，且 `url`/`b64_json` 无 `omitempty` 导致"删不掉键"（`dto/openai_image.go:175-184`）。OpenAI 兼容渠道当前是**完整原样透传**，任何字段丢失都是可见的行为回退。

改写算法（全部经 `common.Unmarshal`/`common.Marshal`，遵守 CLAUDE.md Rule 1）：

1. 顶层解析为 `map[string]json.RawMessage`，保留全部键
2. `data` 解析为 `[]map[string]json.RawMessage`
3. 每个元素只读 `url` / `b64_json` 构造 `ImageSource`（源优先级见 3.1 末尾），转存后按 3.1 **只增删这两个键**，其余键（`revised_prompt` 及任何供应商扩展）原样保留
4. 逐张独立：某张转存失败 → 该张完全不动
5. **`metadata` 处理只针对 Ali**：Ali 把完整上游响应写进顶层 `metadata`（`ali/image.go:268,279`），内含上游直链/base64；但 **MiniMax 的顶层 `metadata` 是上游业务字段**（`minimax/image.go:35,166-172`），无条件删除会与"未知字段无损保留"直接矛盾。因此开关命名为 `GCS_IMAGE_DROP_ALI_METADATA`（默认 `true`），**仅当渠道/API 类型为 Ali 时生效**，其他渠道的 `metadata` 一律保留
6. **实体 header 清理**：改写后 body 与上游 header 不再一致。`IOCopyBytesGracefully` 的白名单只过滤 `Content-Length` 与本地 request-id / provider 内部 header（`service/http.go:45`、复制逻辑 `:103-113`），实体校验类 header 会被原样放行，因此改写路径必须额外删除 `ETag`、`Content-MD5`、`Digest`、`Content-Digest`、`Repr-Digest`、`Content-Encoding`、`Content-Range`、`Last-Modified`，并按新 body 重设 `Content-Length`

### 4.4 GCS 签名 API 改造（`service/gcs_storage.go`）

现状 `GCSSignResultURL(objectName, finishTime)` 把 TTL 钉死为全局 `GCSSignedURLTTL`，签名缓存以 objectName 为唯一 key 且只校验缓存年龄（`:224-230`）。图片要 7 天、视频要 12h，共用缓存会互相污染。

```go
type SignPolicy struct {
    TTL             time.Duration // 期望 TTL
    RetentionAnchor int64         // 保留期锚点（Unix 秒，0 = 不做保留期检查）
    CacheTag        string        // 缓存分区标签；空串 = 不入缓存（见下）
}

// GCSSignObjectURL 现签 V4 GET URL：按 policy 决定 TTL 与保留期收口。
// CacheTag 非空时启用签名缓存，key = objectName|CacheTag|TTL|RetentionAnchor；
// 命中时额外校验缓存的 expiresAt 未超过当前 policy 允许的上限，否则丢弃重签。
func GCSSignObjectURL(objectName string, p SignPolicy) (string, int64, error)

// GCSSignResultURL 保留为视频语义的薄包装（行为不变，现有调用点零改动）。
func GCSSignResultURL(objectName string, finishTime int64) (string, int64, error)
```

保留期收口逻辑（`ttl = min(TTL, 保留期截止 - now - 安全余量)`、余量内直接返回 `ErrGCSResultExpired`）与 `(signedURL, expiresAt)` 二元组语义完全沿用现状，只是参数化。

**缓存生命周期（红线）**：现有 `gcsSignCache` 是**无容量上限的 `sync.Map`**，过期条目只在同一 key 再次访问时才删除、签名后无条件写入（`gcs_storage.go:54,223,244`）。视频/MJ 的对象会被反复读取，缓存有意义；而 `/v1/images/*` 与 Responses 用一次性随机对象名、基本只访问一次，**每张图都会永久留下一个缓存条目 = 无界内存泄漏**。仅改 key 结构不解决这个问题。

规定：

- `/v1/images/*` 与 Responses 的签名 **`CacheTag` 传空串、不入缓存**（本来也没有复用机会）
- 视频（`CacheTag="video"`）与 MJ（`"mj"`）继续使用缓存
- 同时给缓存加**主动淘汰**：写入时若条目数超过 `GCS_SIGN_CACHE_MAX_ENTRIES`（默认 `10000`），触发一次惰性清扫（遍历删除 `signedAt + GCSSignCacheTTL` 已过期的条目）；清扫后仍超限则跳过本次写入（缓存是优化不是正确性依赖）

### 4.5 GCS 就绪状态拆分

现状 `GCSStorageReady()` 同时表达「client 可用」和「视频转存开启」，被视频 worker 当作启动条件（`gcs_transfer.go:156,229`）。若把 client 初始化改成"两开关取或"，只开图片时会意外启动视频 worker。另外 MJ 一旦存了 `gs://`，之后关闭写开关重启就再也读不出历史结果。

| 函数 | 含义 | 调用方 |
|------|------|--------|
| `GCSClientReady()` | client != nil（**读取/签名**可用） | 所有读取侧（MJ 五个出口、签名） |
| `GCSVideoTransferReady()` | `GCSTransferEnabled && client != nil` | 视频 worker（替换 `GCSStorageReady()` 的全部调用点） |
| `GCSImageTransferReady()` | `GCSImageTransferEnabled && client != nil` | 生图三期全部写入侧 |

client 初始化条件：`GCS_TRANSFER_ENABLED || GCS_IMAGE_TRANSFER_ENABLED || GCS_READ_ONLY_ENABLED`。启动期自检（实签探针）与失败 fatal 语义不变。

**运维须知**：两个写开关都关掉但仍需读历史 `gs://` 结果时，必须显式 `GCS_READ_ONLY_ENABLED=true`。已核验 `GCSStorageReady()` 的现有出现仅三处（定义 `gcs_storage.go:94`、`gcs_transfer.go:156`、`:229`），改动时逐一审计。

### 4.6 第 2 期：chat / Responses 里的图片

在 part / item 层做，不做 JSON 正则扫描。

#### 4.6.1 流式写锁（本期最大风险，先解决再动手）

`relay/helper/stream_scanner.go:205-209` 在**整个 dataHandler 回调期间持有 `writeMutex`**；而 ping goroutine 争同一把锁且只等 10 秒（`:155-170`），超时后**标记 `StreamEndReasonPingFail` 并退出 ping goroutine**（keepalive 就此停止；主流不会当场被掐断，但失去心跳后更容易撞上流超时/客户端断连，`:168`、`:285`）。因此在回调里做「下载 + 上传 + 签名」是高风险动作：给 10s 预算恰好等于 ping 等待上限，任何额外开销都会打掉 keepalive。

方案（按优先级）：

1. **首选**：给 `StreamScannerHandler` 增加**锁外预处理钩子** `dataPreHandler(data string) string`，在 `writeMutex.Lock()` 之前执行；Gemini/Responses 的图片转存放在钩子里，锁内只做原来的写 chunk 动作。
2. **退路**（钩子改造被判风险过高时）：`GCS_IMAGE_STREAM_TIMEOUT` 默认 **5s** 且**必须严格小于** scanner 的 10s ping 等待，Gemini 与 Responses 共用该预算；超时即回退原始内容。这只降风险、不根治，须在代码注释里写明。

#### 4.6.2 各出口改造

| 出口 | 改造 |
|------|------|
| Gemini → OpenAI chat 非流式（`relay-gemini.go:1106-1123`） | `part.InlineData` 为 image 时先转存，成功写 `![image](签名URL)`，失败回退原 `data:` URI |
| Gemini → OpenAI chat 流式（`relay-gemini.go:1265-1272`） | 同上，受 4.6.1 约束 |
| Responses **非流式**（`relay_responses.go:20-44`） | 在 `IOCopyBytesGracefully`（`:44`）之前对**原始 JSON** 做 patch：遍历 `output[]`，`type=="image_generation_call"` 且 `result` 非空 → 转存后注入 `result_url`，**`result` 的 base64 原样保留**（官方 SDK 按 base64 解析，改掉即破坏 schema） |
| Responses **流式**（`relay_responses.go:82-91`） | `sendResponsesStreamData`（`:91`）在 switch 之前就写出原始 `data`，patch 必须在它之前完成。处理 `response.output_item.done`（item 为 image_generation_call）与 `response.completed`（`response.output[]`）；`response.image_generation_call.partial_image` 跳过（非终态帧）。同样受 4.6.1 约束 |

Responses 的 DTO（`dto/openai_response.go:340,391`）没有 `result` 字段，实现走**原始 JSON patch**；且必须用 `map[string]json.RawMessage`（不是 `map[string]any`）——后者会把所有数字变成 `float64`，不是无损改写。

#### 4.6.3 Responses 对象命名与去重

同一张图会同时出现在 `output_item.done` 与 `response.completed` 里，必须去重；但**不能用 itemID 当全局对象身份**——`ResponsesOutput.ID` / `ItemID` 都是可空字符串、无全局唯一性保证（`dto/openai_response.go:340,391`），且该 handler 被 XAI 等兼容渠道复用（`xai/adaptor.go:114`）。空 ID 会全部落到同一对象名，配合 `ReuseExisting=true` 直接造成**跨用户串图**。

规定：对象名带请求级命名空间 `{GCS_IMAGE_PREFIX}/resp/{requestID}/{seq}.{ext}`。

**`seq` 的规范主键取 `output_index`**（非流式取 `output[]` 数组下标；流式取事件的 `output_index`）。注意 `OutputIndex` 是**可空指针**（`dto/openai_response.go:399`）——缺失时按"请求内单调递增计数器"分配新 seq，**绝不能把多个 ID/索引都缺失的事件合并成同一张图**。

**去重只在请求内做**：维护 `seq → ImageTransferResult` 的请求级 map（有 `itemID` 时用 `itemID` 做辅助键，把同一 item 的 `output_item.done` 与 `response.completed` 两次出现映射到同一 seq），同一图片只上传一次。`ReuseExisting=true` 仅覆盖同请求内的重复事件与多实例竞写，且按 4.1 做完整性校验。签名 `CacheTag` 传空串（4.4）。

**Gemini chat 的对象命名**（4.6.2 前两行）：`{GCS_IMAGE_PREFIX}/chat/{requestID}/{seq}_{rand4}.{ext}`，`seq` 为该响应内 image part 的出现序号，`ReuseExisting=false`（每次都是新对象，不需要幂等复用），签名 `CacheTag` 同样传空串。

非图片的 `InlineData`（音频等）不动。Gemini 原生格式不动（3.2）。

### 4.7 第 3 期：Midjourney

MJ 有 DB 记录 + 已有代理端点 `/mj/image/{mjId}`（路由 `router/relay-router.go:204`，实现 `relay/mjproxy_handler.go:29-60`），因此能拿到**保留期内有效**的 URL（读时现签）。

#### 4.7.1 两条写入路径（都必须覆盖）

| 路径 | 位置 | 改造 |
|------|------|------|
| 轮询发现成功 | `controller/midjourney.go:140-141`，CAS 落库 `:176`（`model/midjourney.go:160`） | 转存后 `task.ImageUrl = gs://...`；失败保留上游直链。不新增写者、沿用既有 CAS |
| **`code=21` 直接成功**（提交接口发现任务已存在且已出图） | `relay/mjproxy_handler.go:599-618`，随后 `Insert()` `:626` | 同样转存后再 `Insert`。**必须覆盖**：该分支直接把 `Progress` 置 `100%`，而轮询集合是 `progress != '100%'`（`model/midjourney.go:93`），此类任务永远不会进入轮询转存 |

四条硬约束：

1. **独立 context**：轮询的局部 `ctx` 在 `controller/midjourney.go:119` 就已 `cancel()`，逐任务处理从 `:121` 才开始——复用它会让转存**立即被取消**。必须 `context.WithTimeout(context.Background(), ...)` 新建。
2. **写入所有权不变：CAS 与退款仍归轮询 goroutine**（`controller/midjourney.go:176` CAS、`:179-181` 仅 CAS winner 退款）。转存**只是 CAS 前的一次取值改写**，不接管 CAS、不接管退款、不新增写者。明确否决"worker 自己重载 task + CAS + 退款"的模型——那会把资损所有权搬到新代码里，收益不抵风险。
3. **批内并发转存（首版就要做）**：MJ 只有一个 master 无限循环（`controller/midjourney.go:23`、`main.go:133`），按渠道、任务串行。做法是：本轮先收集**本轮新变为成功且需转存**的任务，用 `errgroup` + 信号量（上限 `GCS_IMAGE_CONCURRENCY`）并发转存，全部返回后再按原顺序逐个走既有 CAS 落库。
   - 阻塞上界 = `ceil(S / GCS_IMAGE_CONCURRENCY) × GCS_IMAGE_MJ_TIMEOUT`，其中 **S = 本轮新成功任务数**（不是轮询集合大小 N）。默认 `4` 并发、`30s` 预算下，单轮新成功 8 张的上界是 60s，可接受
   - 该上界是**显式接受的代价**，须在 4.11 用「MJ 单轮转存阻塞时长」指标监控；超出预期再考虑改异步 worker（届时必须连带重新设计 CAS/退款所有权）
   - `code=21` 是**用户同步请求路径**（`mjproxy_handler.go:599-626`），不进这个批队列——它必须在 `Insert()` 前就地完成转存；但**共享同一个全局信号量**，避免 MJ 高峰时轮询与提交路径叠加打满带宽
4. **幂等**：多实例可能在 CAS 前重复上传 → 固定对象名 `{GCS_IMAGE_PREFIX}/mj/{mjId}.{ext}` + `ReuseExisting=true` + 完整性校验（4.1）。写入阶段 `Sign=false`（只存 `gs://`）。

`code=21` 分支写给客户端的提交响应体（`mjproxy_handler.go:647`）是**上游原始 body**（仅 patch 了 `code`），其中 `properties.imageUrl` 仍是上游直链。**本期不改该响应体**（提交回执而非结果出口，MJ 客户端按 fetch 取图）——列为已知残余泄露点。

#### 4.7.2 全部读取出口（缺一个就泄露裸 `gs://`）

| 出口 | 位置 | 改造 |
|------|------|------|
| `/mj/image/{mjId}` 代理 | `relay/mjproxy_handler.go:29-60` | `gs://` → **302 到现签 URL**；超保留期 → 410；签名失败 → 503。**分支位置：在 `GetByOnlyMJId` 的 task nil 检查之后、渠道查询之前**（即插在 `:37` 与 `:38` 之间），从而先于代理客户端初始化（`:38-49`，无效代理会直接 400）与 SSRF 校验（`:53`）——历史 `gs://` 对象既不依赖渠道也不依赖代理。视频入口已是这个顺序（`controller/video_proxy.go:61`） |
| fetch 单个任务 | `mjproxy_handler.go:141-149` | 见下 helper 语义 |
| fetch 批量/按条件 | `mjproxy_handler.go:354-357` | 同上 |
| 管理员任务列表 | `controller/midjourney.go:257-280` | 同上 |
| 用户任务列表 | `controller/midjourney.go:282-300` | 同上 |

统一 helper `service.MjDisplayImageURL(task) string`，语义写死以避免代理自循环：

- `MjForwardUrlEnabled == true` → 返回代理 URL（`ServerAddress + /mj/image/ + MjId`），**不现签**（现签发生在 `/mj/image` 内部）
- `MjForwardUrlEnabled == false` 且 `ImageUrl` 为 `gs://` → 现签 URL；签名失败/过期 → 退回代理 URL
- 其余 → 原样返回

`/mj/image` 内部**不调用该 helper**，直接用底层 `GCSSignObjectURL`（CacheTag `"mj"`）。

#### 4.7.3 时间单位（资损级细节）

MJ 的 `FinishTime` 是**毫秒**（`controller/midjourney.go:140` 抄上游值、`relay/mjproxy_handler.go:611` 为 `UnixNano()/int64(time.Millisecond)`），而 `GCSSignObjectURL` 的保留期锚点是 **Unix 秒**（`gcs_storage.go:211` 的 `finishTime > 0` 判断 + `time.Unix(finishTime, 0)`）。直接传入会让保留期截止落到几万年后，过期检查形同失效、必然签出死链，且不会有任何报错。

规定三条，**锚点必须稳定（不能每次读取都用 `now`）**：

1. **写入侧一次性补齐并持久化**：转存成功、终态 CAS/`Insert` 之前，若上游 `FinishTime == 0`（轮询会原样抄上游零值，`controller/midjourney.go:140`）则填入当前毫秒。这样锚点写死在 DB 里。
2. **读取侧只做 `/1000`**，绝不用 `now` 兜底——否则保留期会随每次读取不断向后滑动，等于永不过期，30 天后必然签出 404 死链。
3. **历史数据（`gs://` 且 `FinishTime == 0`）**：用稳定替代锚点 `SubmitTime`（`model/midjourney.go:13`，毫秒，成功任务必然非零）；若 `SubmitTime` 也为 0 则**拒签**（读取出口按过期处理：`/mj/image` 返 410，JSON 出口退回代理 URL）。

三条都要有单测钉死，尤其"同一任务连续两次读取拿到的保留期截止相同"。

`midjourney` 表的 `VideoUrl`/`VideoUrls`（MJ 视频）沿用视频设计的排除结论，本期不动。不新增列、不改 schema（`ImageUrl` 是既有列），三库天然兼容（CLAUDE.md Rule 2）。

### 4.8 配置项（`setting/gcs_setting.go` 扩展）

| 配置 | 默认 | 说明 |
|------|------|------|
| `GCS_IMAGE_TRANSFER_ENABLED` | `false` | 生图转存总开关，独立于视频 |
| `GCS_READ_ONLY_ENABLED` | `false` | 两个写开关都关时仍初始化 client，以便读历史 `gs://`（4.5） |
| `GCS_IMAGE_PREFIX` | `api/image` | 同 bucket 不同前缀 |
| `GCS_IMAGE_SIGNED_URL_TTL` | `168h`（7 天） | V4 上限；无记录不能重签。**解析时须钳制到 ≤168h**（现有 `getEnvDuration` 只校验正数，`setting/gcs_setting.go:60-73`），超限值 GCS 会直接拒签 |
| `GCS_SIGN_CACHE_MAX_ENTRIES` | `10000` | 签名缓存条目上限 + 惰性清扫阈值（4.4） |
| `GCS_IMAGE_MAX_SIZE` | `32MiB` | 单图体积上限 |
| `GCS_IMAGE_TRANSFER_TIMEOUT` | `60s` | 同步 HTTP 路径单图预算 |
| `GCS_IMAGE_STREAM_TIMEOUT` | `5s` | 流式路径预算，**必须严格小于** scanner 的 10s ping 等待（4.6.1） |
| `GCS_IMAGE_MJ_TIMEOUT` | `30s` | MJ 后台转存单图预算（4.7.1） |
| `GCS_IMAGE_CONCURRENCY` | `4` | 并发转存数（响应内 / MJ worker 池共用该上限语义） |
| `GCS_IMAGE_CAPTURE_MAX` | `64MiB` | 响应缓冲上限，超过切 passthrough |
| `GCS_IMAGE_STRIP_B64_WHEN_URL` | `false` | 未传 `response_format` 且转存成功时删除 `b64_json`（响应瘦身，行为变更） |
| `GCS_IMAGE_DROP_ALI_METADATA` | `true` | **仅 Ali 渠道**：转存成功时删除顶层 `metadata`（防上游直链泄露）；其他渠道 `metadata` 一律保留 |

`GCS_IMAGE_TRANSFER_ENABLED` 关闭时：不安装缓冲 writer、Gemini/Responses/MJ **写入侧**全走原逻辑（零开销、零行为变化）；**但读取侧仍会对历史 `gs://` 现签**（前提是 client 可用，见 4.5）——否则存量 MJ 结果会变成裸 `gs://` 泄露给用户。

### 4.9 失败与降级（决策 4 的落地）

| 失败点 | 行为 |
|--------|------|
| 上游图下载失败 / SSRF 拦截 / 超时 | 该张保留上游原值，其余照常转存 |
| base64 解码失败 / MIME 不在白名单 | 同上 |
| 超体积上限 | 同上（上传经 cancel context 放弃，绝不留半截对象） |
| GCS 上传失败 / 凭证失效 / 对象已存在且 `ReuseExisting=false` / 完整性校验不通过 | 同上 |
| 签名失败 | 同上（对象已上传，属浪费但不影响用户） |
| 响应体解析失败 / 非 JSON / 非 200 / 流式 / 缓冲超限 | 整体 passthrough，不改写 |
| 全部图片转存失败 | 响应与今天完全一致（等价于开关关闭） |
| **最终写客户端失败** | 只记 error 日志 + 照常结算，**不转 relay error**（决策 8 / 4.2.3） |
| MJ 转存失败 | `ImageUrl` 保留上游直链，走原有代理/直出逻辑 |

所有失败打 error 级日志（含 kind 分类）+ 计入指标，**不重试**（同步路径，重试只放大用户等待），**绝不转成 relay error**。

### 4.10 计费与日志（不得触碰）

真实顺序：定价 `ModelPriceHelper`（`controller/relay.go:153`）→ **预扣 `PreConsumeBilling`（`controller/relay.go:164`）** → 渠道重试循环（`:190`）→ `DoResponse`（`relay/image_handler.go:114`）→ `n` 修正（`:121-134`）→ `PostTextConsumeQuota`（`:160`）。退款只发生在**所有重试都失败后**的 defer 里（`controller/relay.go:169-175`）。quota 用 `PriceData.OtherRatios`（`service/text_quota.go:304-321`）。

红线：

- `n` 表示**生成/请求数量**，绝不能按"转存成功张数"修改（`relay/image_handler.go:121` 只在尚未存在时补入）
- 转存层不得修改 `PriceData`、不得返回 relay error —— 否则触发渠道重试，最终仍失败还会退款。决策 4 因此是**计费正确性**的前提，不只是体验取舍
- 未发现按响应体字节数计费的逻辑，url/base64 替换不影响金额
- **响应耗时会包含转存时间**（`service/text_quota.go:182` 计算，`:491` 写入消费日志），耗时口径变化需通告

### 4.11 可观测性（`service/gcs_metrics.go` 扩展）

- 生图转存耗时直方图（按入口 + 渠道类型）——直接构成用户感知延迟
- 结果计数器：`success` / `exists-reuse` / `download-fail` / `decode-fail` / `mime-reject` / `oversize` / `gcs-auth-fail` / `gcs-service-fail` / `sign-fail` / `corrupt-object` / `passthrough`
- 回退率（fallback / total）——GCS 或上游 CDN 异常的第一信号
- 字节吞吐（下载 + 上传）——出口带宽与费用校准
- 流式路径转存耗时单列 + `>=GCS_IMAGE_STREAM_TIMEOUT` 的次数（直接关联 ping fail 风险）
- MJ 转存队列深度与单轮阻塞时长（4.7.1 的并发池校准依据）

### 4.12 对外契约

- `/v1/images/*` 与 Responses 返回的签名 URL **7 天有效**，过期后无法刷新（无任务记录）。需长期持有请用 `response_format=b64_json` 或自行落库
- MJ 的 `/mj/image/{mjId}` 在保留期（`GCS_RESULT_RETENTION_DAYS`，默认 30 天）内有效，过期返回 410
- 结果保留期由 bucket 生命周期规则决定，必须与 `GCS_RESULT_RETENTION_DAYS` 一致
- 返回的 URL 不保证稳定也不保证每次不同（签名缓存命中期内相同），客户端不应做 URL 等值比较
- **Gemini 原生格式入口的图片仍为内联 base64，不经自建 CDN**（3.2）

## 5. 实现清单

| # | 期 | 改动 | 文件 |
|---|----|------|------|
| 1 | 1 | `GCSSignObjectURL` + `SignPolicy`（TTL 参数化、缓存 key 含 policy、命中校验 expiresAt），`GCSSignResultURL` 降级为视频薄包装 | `service/gcs_storage.go` |
| 2 | 1 | 就绪状态拆三态 + client 初始化条件 + **审计 `GCSStorageReady()` 三处调用点** | `service/gcs_storage.go`、`service/gcs_transfer.go` |
| 3 | 1 | `DoDownloadRequestWithContext`（Worker 与非 Worker **两条分支都绑 context**），保留旧包装 | `service/download.go` |
| 4 | 1 | 转存原语 `TransferImage`/`TransferImages`（图片 MIME 白名单、体积上限、TeeReader 取 base64、`ReuseExisting` + 完整性校验、并发、per-path 超时） | `service/gcs_image.go`（新增） |
| 5 | 1 | 配置项 + 开关 | `setting/gcs_setting.go` |
| 6 | 1 | 缓冲 writer（完整 `gin.ResponseWriter` + `Unwrap`、**shadow header 事务隔离**、吞 Flush、SSE 三处检查点、Hijack 先 commit、缓冲超限切 passthrough、`committed` 标记） | `relay/helper/capturing_writer.go`（新增） |
| 7 | 1 | `ImageHelper` 收口 + 无损 raw JSON 改写 + 实体 header 清理 + 错误路径丢弃缓冲 + 提交失败只记日志 | `relay/image_handler.go` |
| 8 | 1 | 指标扩展 | `service/gcs_metrics.go` |
| 9 | 1 | 单测：3.1 三种 `response_format` × 上游 url/base64；两者兼有时的源优先级；**未知字段/顶层 usage/MiniMax metadata 不丢**；Ali metadata 按渠道删；`url`/`b64_json` 键真删除；转存失败逐张回退；`DoResponse` error → 丢弃缓冲且底层 header 不变；**最终写客户端失败仍完成结算且不返回 relay error**；Flush 被吞；SSE 三处检查点直通；缓冲超限直通；Content-Length 与 header 清理正确；**签名 `CacheTag` 为空时不写缓存 + 超限惰性淘汰**；`ReuseExisting` 命中 412 后 drain 取精确 CRC 校验、drain 失败不复用 | `relay/image_handler_test.go`、`relay/helper/capturing_writer_test.go`、`service/gcs_image_test.go`（新增） |
| 10 | 2 | **scanner 锁外预处理钩子**（首选方案，4.6.1） | `relay/helper/stream_scanner.go` |
| 11 | 2 | Gemini chat 非流式 + 流式 inline image → 签名 URL（失败/超时回退 data URI） | `relay/channel/gemini/relay-gemini.go` |
| 12 | 2 | Responses 非流式 + 流式注入 `result_url`（`map[string]json.RawMessage` patch、`result` 不动、requestID 命名空间 + 请求内去重） | `relay/channel/openai/relay_responses.go` |
| 13 | 3 | MJ 两条写入路径转存（轮询 + `code=21`）：独立 context、有界并发 worker、固定对象名 + `ReuseExisting` | `controller/midjourney.go`、`relay/mjproxy_handler.go` |
| 14 | 3 | MJ 五个读取出口统一 `MjDisplayImageURL`；`/mj/image` 的 `gs://` 分支置于渠道/代理/SSRF 之前；**FinishTime 毫秒→秒 + 0 值补齐**（含单测） | `relay/mjproxy_handler.go`、`controller/midjourney.go`、`service/gcs_image.go` |
| 15 | 2/3 | 单测：Responses 的 `output_index`/`itemID` 缺失与重复时对象名不碰撞、双事件只上传一次；scanner 锁外钩子在锁外执行；MJ 批内并发有上界且 `code=21` 共享同一信号量；**无效渠道代理时 `gs://` 分支仍优先命中**；**同一 MJ 任务连续两次读取的保留期截止相同**（稳定锚点）；`FinishTime==0` 历史数据用 `SubmitTime`、两者皆 0 时拒签 | 对应各包 `_test.go` |
| 16 | — | 代码注释：MJ notify 路由（`mjproxy_handler.go:111-123`）恢复时必须接入转存 | `relay/mjproxy_handler.go` |
| 17 | — | 用户文档 + CHANGELOG：7 天 TTL 契约、`response_format` 适配表、MJ 链接语义、Gemini 原生排除、`/v1/edits` 非生图入口、错误路径语义变化、耗时口径变化 | 文档 |

预估规模：第 1 期 ~700 行（含测试与 GCS API 改造），第 2 期 ~300 行（含 scanner 钩子），第 3 期 ~300 行。

## 6. 风险与注意事项

1. **缓冲 writer 是本设计最高风险面**：body/status/header 三者的事务边界必须同时正确——buffered 丢弃时底层 header 不能被污染，passthrough 后不能重复写，任何返回路径都要恢复 writer。红线是"用户拿到空响应而额度已扣"。必须 defer 兜底 + 清单项 9 的全部错误路径单测。
2. **流式写锁**：`stream_scanner.go:205-209` 的回调持写锁、ping 只等 10s（`:155-170`）。不做锁外钩子就只能靠 <10s 预算降风险，不能根治；上线后须盯 4.11 的流式超时计数。
3. **响应延迟增加**（固有代价）：生图响应时间 = 上游生成 + 下载 + 上传 + 签名。单张 1-4 MB 同区约几百 ms，跨境上游直链可能到秒级。多图并发（默认 4）后与单图同阶。
4. **内存驻留高于直觉**：OpenAI 与 Gemini 本来就先 `ReadAll` 整个响应（`relay-openai.go:558`、`relay-gemini.go:1625`），再叠加缓冲 writer 副本、raw JSON map、解码后图片字节、TeeReader 缓冲与 marshal 结果，峰值可达图片大小的 **3-5 倍**。由 `GCS_IMAGE_MAX_SIZE`(32 MiB) + `GCS_IMAGE_CAPTURE_MAX`(64 MiB) 双重约束，**上线前必须做大图并发压测**。
5. **`GCSStorageReady()` 语义拆分的连带风险**：改错会让只开图片时启动视频 worker，或让 MJ 历史结果读不出来。必须逐调用点审计（清单项 2）。
6. **MJ FinishTime 毫秒/秒与 0 值**：见 4.7.3，错了会使保留期检查失效、签出必 404 死链，且无任何报错。单测钉死。
7. **Responses 对象命名**：若退回"itemID 当对象名"，空 ID 或 ID 重复会造成**跨用户串图**——这是本设计里唯一的数据泄露级陷阱，命名规则不得简化（4.6.3）。
8. **MJ 轮询阻塞**：单 master 串行循环，转存必须走有界并发池 + 独立 context，否则拖慢所有 MJ 状态更新与失败退款（4.7.1）。
9. **`/v1/images/*` 与 Responses 的 7 天死链**：无记录 → 无法重签。需要永久链接得另立项加「图片结果表 + 代理端点」（与 MJ 同构），本期不做。
10. **行为变更面**：`GCS_IMAGE_STRIP_B64_WHEN_URL=true` 会让未传 `response_format` 的客户端丢 `b64_json`；`GCS_IMAGE_DROP_ALI_METADATA=true` 会让 Ali 客户端丢顶层 `metadata`。前者默认关、后者默认开，都需通告。
11. **出口带宽与费用**：每张图经网关下载 + 上传各一次。生图 QPS 通常远高于视频，费用量级需先按 4.11 的字节吞吐指标观察再放量。
12. **已知残余泄露点**（本期接受）：
    - MJ `code=21` 提交回执（`mjproxy_handler.go:647`）—— 非结果出口，客户端按 fetch 取图
    - 关闭 `GCS_IMAGE_DROP_ALI_METADATA` 时的 Ali `metadata` —— 它**就是 `/v1/images/*` 的顶层响应字段**（`ali/image.go:268,279`），不是"非结果出口"；这是为兼容可能依赖该字段的客户端而留的开关，打开开关（默认）即无泄露
13. **不新增 DB 字段、不改 schema**（三库兼容，Rule 2）；所有 JSON 走 `common.Marshal`/`common.Unmarshal`（Rule 1）。

## 7. 评审记录

### 第 1 轮（2026-08-06，codex 只读核验）

7 条阻断问题，全部经人工复核确认属实并修订：

1. 缓冲 writer 把 `Flush()` 当透传信号会让主流渠道全部绕过改写（`service/http.go:126` 无条件 Flush）→ buffered 模式吞 Flush（4.2.1）
2. `DoResponse` 返回错误时刷出缓冲会与外层重试循环冲突（`controller/relay.go:190`、`:89`）→ 改为丢弃缓冲（4.2.2）
3. 走 `dto.ImageResponse` 重建响应会丢字段且删不掉键 → raw JSON 无损改写（4.3）
4. 签名 TTL 全局化 + 缓存 key 不含 policy + 就绪状态耦合 → 拆 `SignPolicy` 与三态就绪（4.4 / 4.5）
5. MJ `code=21` 路径绕过轮询，且 `FinishTime` 是毫秒 → 补写入路径 + 单位转换（4.7）
6. Responses DTO 无 `result` 字段、流式先写原始 data、同图重复出现 → raw JSON patch + 去重（4.6）
7. Gemini 原生入口未覆盖 → 明确写成排除项与理由（3.2）

另采纳 10 条建议：实体 header 清理、图片专用 MIME 映射、context 版下载函数、放弃随机后缀重试、完整 `gin.ResponseWriter` 实现、内存估算上修、流式持锁超时收紧、渠道覆盖表修正、MJ 五个读取出口、计费顺序与红线。

### 第 2 轮（2026-08-06，codex 只读核验修订稿）

7 条阻断问题，全部经人工复核确认属实并修订：

1. 缓冲 writer 未定义 **header 事务隔离** → shadow header 三段语义（4.2.1）
2. 写客户端失败的语义未定义（buffered 下 `Write` 不暴露 I/O 错误，错误延迟到最终提交；Jimeng `jimeng/image.go:84-86`、MiniMax `minimax/image.go:208-210` 是现存"写后返错"分支）→ 新增决策 8 与 4.2.3
3. **`GCS_IMAGE_DROP_METADATA` 会误删 MiniMax 的业务 metadata**（`minimax/image.go:35,166-172`）→ 改为 `GCS_IMAGE_DROP_ALI_METADATA`，仅 Ali 生效（4.3）
4. Responses 用 itemID 当固定对象名有**跨用户串图**风险（ID 可空、无唯一性保证、handler 被 XAI 等复用）→ requestID 命名空间 + 请求内去重 + 完整性校验（4.6.3）
5. Gemini/Responses 都在 scanner 写锁内上传，10s 预算恰等于 ping 超时（`stream_scanner.go:155-170,205-209`）→ 首选锁外预处理钩子，退路预算收到 5s（4.6.1）
6. MJ 复用轮询局部 ctx 会**立即被取消**（`controller/midjourney.go:119` 已 cancel，任务循环从 `:121` 开始）；单 master 串行会造成 `N×60s` 阻塞 → 独立 context + 首版就用有界并发池（4.7.1）
7. `/mj/image` 的 `gs://` 分支放在 SSRF 前仍不够早——渠道查询与代理初始化在更前面且无效代理直接 400（`mjproxy_handler.go:38-49`）→ 分支紧跟 `GetByOnlyMJId`（4.7.2）

另采纳 12 条建议：逻辑 `WriteHeaderNow`/SSE 三处检查点/`Hijack` 先 commit/`Unwrap`、SignPolicy 缓存 key 含 TTL 与 anchor、header 清理补 `Content-Digest`/`Repr-Digest`/`Content-Range`/`Last-Modified`、Worker 下载分支也绑 context、MJ `FinishTime==0` 补齐、`MjDisplayImageURL` 与 `MjForwardUrlEnabled` 关系写死避免代理自循环、`/v1/edits` 非生图入口说明、MJ notify 恢复时须接入转存、Responses patch 用 `json.RawMessage` 而非 `any`、data 元素同时有 url 与 b64 时的源优先级、计费文字与行号修正、开关关闭时读取侧仍需现签（消除 4.8 自相矛盾）。

### 第 3 轮（2026-08-06，codex 只读核验二稿）

确认第 2 轮 7 条中 5 条已修订到位；4 条**必须动手前定死**的缺口，全部经人工复核确认属实并修订：

1. **签名缓存对一次性对象无界增长**：`gcsSignCache` 是无容量上限的 `sync.Map`、过期条目只在同 key 再访问时才删（`gcs_storage.go:54,223,244`），而 `/v1/images/*` 用一次性随机对象名 → 每张图永久留一条 = 内存泄漏。且 `TransferOpts` 无法表达"不签名"（MJ 写入阶段只需存 `gs://`）→ 新增 `Sign`/`SignPolicy` 字段、`CacheTag` 空串即不入缓存、缓存加容量上限与惰性淘汰（4.1 / 4.4 / 4.8）
2. **`ReuseExisting` 的完整性校验未闭合**：原文一边禁止"只看 Size>0"、一边又允许"未读完时退化为 Size>0"，自相矛盾；而 412 确实可能在 `io.Copy` 阶段就返回（`gcs_storage.go:150`）→ 规定 412 后 drain counting reader 至 EOF 取精确 size/CRC 再校验，drain 不成即回退、禁止复用（4.1）
3. **MJ worker 池与 CAS/退款所有权未定义**：原文同时要求"不新增写者沿用既有 CAS"和"有界工作队列"，两者未调和 → 明确选定「批内并发转存 + 轮询 goroutine 保留 CAS 与退款所有权」，写明 `ceil(S/并发) × 预算` 的阻塞上界（S = 本轮新成功数）与 `code=21` 共享信号量（4.7.1）
4. **`FinishTime==0` 补齐时机会造成滑动锚点**：若读取侧用 `now` 兜底，保留期每次读取都往后滑 = 永不过期，30 天后必签死链 → 改为写入侧一次性补齐并持久化、读取侧只 `/1000`、历史零值用 `SubmitTime`（`model/midjourney.go:13`）、两者皆 0 则拒签（4.7.3）

另采纳 7 条建议并修正 14 处行号引用：scanner ping 超时的准确后果（停 keepalive 而非当场掐流）、Responses 去重以 `output_index` 为规范主键（可空指针时分配请求内单调 seq）、补 Gemini chat 对象命名、`/mj/image` 分支位置表述精确到"task nil 检查之后、渠道查询之前"、`GCS_IMAGE_SIGNED_URL_TTL` 解析时钳制 ≤168h、Ali `metadata` 定性改为"结果响应中的兼容字段"（它确实是 `/v1/images/*` 顶层字段）、测试清单补 6 项。

codex 明确结论：这 4 条修完**无需再做全量评审，可直接进入实现并用清单测试验收**。
