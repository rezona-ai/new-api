package helper

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CapturingWriter 缓冲型 gin.ResponseWriter：把 handler 写出的响应体截留在内存里，
// 供调用方改写后再提交。用于生图响应的统一收口
//（设计文档 docs/superpowers/specs/2026-08-06-image-gen-cdn-design.md 4.2.1）。
//
// 三条不可违反的规则：
//
//  1. **buffered 模式吞掉 Flush()**。service.IOCopyBytesGracefully 对普通非流式
//     JSON 也会无条件调用 c.Writer.Flush()（service/http.go:126），若把 Flush 当作
//     "必须立刻透传"，OpenAI/Ali/Zhipu 等主流图片渠道会全部绕过改写。流式与否在
//     安装本 writer 之前就已由 Content-Type 判定（relay/image_handler.go:97-100）。
//
//  2. **header 事务隔离**。安装时 clone 底层 header 到 shadow；buffered 阶段所有
//     header 读写只作用于 shadow；提交/切直通时才同步回底层；Discard 时底层 header
//     一个字节都不改动——否则渠道尝试设的 Content-Type/ETag 会污染外层的渠道重试
//     （controller/relay.go:190）与统一错误响应（controller/relay.go:89）。
//
//  3. **切直通时先补写已缓冲内容**，此后写入直达底层并标记 committed，绝不丢字节。
type CapturingWriter struct {
	gin.ResponseWriter

	shadowHeader http.Header
	buf          bytes.Buffer
	captureMax   int64

	status    int
	size      int
	written   bool // 逻辑上是否已写响应（WriteHeaderNow/Write/Flush 之后为真，与 gin 语义一致）
	committed bool // 是否已把响应交给底层 writer（此后不可改写）
}

// 编译期确认接口完整实现。
var _ gin.ResponseWriter = (*CapturingWriter)(nil)

// NewCapturingWriter 包装一个 gin.ResponseWriter。captureMax <= 0 表示不限制缓冲大小。
func NewCapturingWriter(w gin.ResponseWriter, captureMax int64) *CapturingWriter {
	cw := &CapturingWriter{
		ResponseWriter: w,
		shadowHeader:   make(http.Header, len(w.Header())+4),
		captureMax:     captureMax,
		status:         http.StatusOK, // 与 gin 的 defaultStatus 一致
	}
	for k, v := range w.Header() {
		cw.shadowHeader[k] = append([]string(nil), v...)
	}
	return cw
}

// Unwrap 返回底层 writer，供调用方在 Discard 后恢复 c.Writer。
func (cw *CapturingWriter) Unwrap() gin.ResponseWriter { return cw.ResponseWriter }

// Committed 是否已提交（已把字节交给底层，不可再改写）。
func (cw *CapturingWriter) Committed() bool { return cw.committed }

// Body 返回缓冲中的响应体（提交前有效）。
func (cw *CapturingWriter) Body() []byte { return cw.buf.Bytes() }

// CapturedStatus 返回 handler 写入的状态码。
func (cw *CapturingWriter) CapturedStatus() int { return cw.status }

// Header buffered 阶段返回 shadow header；提交后直接返回底层 header。
func (cw *CapturingWriter) Header() http.Header {
	if cw.committed {
		return cw.ResponseWriter.Header()
	}
	return cw.shadowHeader
}

func (cw *CapturingWriter) Status() int { return cw.status }

func (cw *CapturingWriter) Size() int { return cw.size }

func (cw *CapturingWriter) Written() bool { return cw.written }

// WriteHeader 与 gin 的 responseWriter 语义一致：只记录状态码，不代表"已写响应"
//（gin 的 Written() 是 size != -1，只有 WriteHeaderNow/Write 才置真）。
func (cw *CapturingWriter) WriteHeader(code int) {
	if cw.committed {
		cw.ResponseWriter.WriteHeader(code)
		return
	}
	if code > 0 {
		cw.status = code
	}
	cw.switchIfEventStream()
}

// WriteHeaderNow buffered 阶段只做"逻辑上已写头"（置 written，不透到底层）。
func (cw *CapturingWriter) WriteHeaderNow() {
	if cw.committed {
		cw.ResponseWriter.WriteHeaderNow()
		return
	}
	cw.written = true
	cw.switchIfEventStream()
}

func (cw *CapturingWriter) Write(data []byte) (int, error) {
	if !cw.committed {
		cw.written = true
		cw.switchIfEventStream()
	}
	if cw.committed {
		n, err := cw.ResponseWriter.Write(data)
		cw.size += n
		return n, err
	}
	if cw.captureMax > 0 && int64(cw.buf.Len()+len(data)) > cw.captureMax {
		// 超过缓冲上限：放弃改写，先补写已缓冲内容再直通
		if err := cw.switchToPassthrough(); err != nil {
			return 0, err
		}
		n, err := cw.ResponseWriter.Write(data)
		cw.size += n
		return n, err
	}
	n, err := cw.buf.Write(data)
	cw.size += n
	return n, err
}

func (cw *CapturingWriter) WriteString(s string) (int, error) {
	return cw.Write([]byte(s))
}

// Flush buffered 阶段被吞掉（见类型注释规则 1），但仍执行"逻辑 WriteHeaderNow"
//（gin 的 Flush 也是先 WriteHeaderNow 再 flush）；已提交则透传到底层。
func (cw *CapturingWriter) Flush() {
	if cw.committed {
		cw.ResponseWriter.Flush()
		return
	}
	cw.written = true
	cw.switchIfEventStream()
}

// Hijack 先提交再交出连接——劫持后我们无法再改写任何字节。
func (cw *CapturingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !cw.committed {
		if err := cw.switchToPassthrough(); err != nil {
			return nil, nil, err
		}
	}
	return cw.ResponseWriter.Hijack()
}

// switchIfEventStream SSE 响应无法缓冲改写，识别到就立即切直通。
// 三个检查点（WriteHeader / WriteHeaderNow / 首次 Write）都要调用：
// 渠道未必显式调用 WriteHeader。
func (cw *CapturingWriter) switchIfEventStream() {
	if cw.committed {
		return
	}
	if strings.HasPrefix(strings.ToLower(cw.shadowHeader.Get("Content-Type")), "text/event-stream") {
		_ = cw.switchToPassthrough()
	}
}

// switchToPassthrough 同步 shadow header 到底层、按序补写已缓冲字节，然后标记已提交。
func (cw *CapturingWriter) switchToPassthrough() error {
	if cw.committed {
		return nil
	}
	cw.syncHeader(nil)
	cw.committed = true
	cw.ResponseWriter.WriteHeader(cw.status)
	if cw.buf.Len() > 0 {
		if _, err := cw.ResponseWriter.Write(cw.buf.Bytes()); err != nil {
			return fmt.Errorf("capturing writer: flush buffered body failed: %w", err)
		}
		cw.buf.Reset()
	}
	return nil
}

// Commit 原样提交缓冲内容。已提交时为 no-op。
func (cw *CapturingWriter) Commit() error {
	return cw.CommitBody(nil, nil)
}

// CommitBody 提交响应。body 非 nil 时替换缓冲内容并按新长度重设 Content-Length；
// headerMutator 非 nil 时在同步 header 之前对 shadow header 做最后修改
//（生图改写用它删除 ETag/Content-MD5 等实体校验 header）。
// 已提交时为 no-op（防止重复写响应）。
func (cw *CapturingWriter) CommitBody(body []byte, headerMutator func(http.Header)) error {
	if cw.committed {
		return nil
	}
	payload := cw.buf.Bytes()
	if body != nil {
		payload = body
	}
	cw.syncHeader(func(h http.Header) {
		if headerMutator != nil {
			headerMutator(h)
		}
		h.Set("Content-Length", fmt.Sprintf("%d", len(payload)))
	})
	cw.committed = true
	cw.ResponseWriter.WriteHeader(cw.status)
	if len(payload) > 0 {
		if _, err := cw.ResponseWriter.Write(payload); err != nil {
			return fmt.Errorf("capturing writer: write response body failed: %w", err)
		}
	}
	cw.buf.Reset()
	return nil
}

// Discard 丢弃缓冲，**底层 header 与 body 完全不动**——外层的渠道重试与统一错误
// 响应需要一个干净的 writer。已提交时无法回滚，返回后调用方应视为响应已发出。
func (cw *CapturingWriter) Discard() {
	if cw.committed {
		return
	}
	cw.buf.Reset()
	cw.size = 0
	cw.written = false
}

// syncHeader 把 shadow header 同步到底层 writer（先应用 mutator）。
func (cw *CapturingWriter) syncHeader(mutator func(http.Header)) {
	if mutator != nil {
		mutator(cw.shadowHeader)
	}
	dst := cw.ResponseWriter.Header()
	for k := range dst {
		delete(dst, k)
	}
	for k, v := range cw.shadowHeader {
		dst[k] = append([]string(nil), v...)
	}
}
