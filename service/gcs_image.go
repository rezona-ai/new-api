package service

import (
	"net/http"
	"strings"
)

// 生图结果转存 GCS：转存原语（设计文档 docs/superpowers/specs/2026-08-06-image-gen-cdn-design.md 4.1）。
//
// 与视频转存的本质差异：生图是同步请求，转存必须在响应下发前完成，没有状态机、
// 没有退款语义。因此本文件的任何失败都只返回 error，由调用方回退透传上游原始结果——
// 绝不允许转成 relay error（会触发渠道重试，最终失败还会退款）。

// gcsImageExtByMime 图片 MIME → 对象扩展名白名单。
// 扩展名一律取自该映射，禁止把上游 URL 的路径/查询串拼进对象名（设计 4.1）。
var gcsImageExtByMime = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// gcsImageMimeAliases 归一化上游的非规范 MIME 写法。
var gcsImageMimeAliases = map[string]string{
	"image/jpg": "image/jpeg",
}

// normalizeImageMime 去掉参数、转小写并归一别名；不在白名单内返回 ""。
func normalizeImageMime(raw string) string {
	mime := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if alias, ok := gcsImageMimeAliases[mime]; ok {
		mime = alias
	}
	if _, ok := gcsImageExtByMime[mime]; !ok {
		return ""
	}
	return mime
}

// resolveImageMime 按 上游 Content-Type → 调用方 hint → 内容嗅探 的顺序判定图片
// MIME 与对象扩展名。三者都无法判定（或判出非白名单类型）时 ok=false，
// 调用方按转存失败处理并回退透传——绝不上传未知类型。
func resolveImageMime(upstreamCT, hint string, head []byte) (string, string, bool) {
	for _, candidate := range []string{upstreamCT, hint} {
		if mime := normalizeImageMime(candidate); mime != "" {
			return mime, gcsImageExtByMime[mime], true
		}
	}
	if len(head) > 0 {
		if mime := normalizeImageMime(http.DetectContentType(head)); mime != "" {
			return mime, gcsImageExtByMime[mime], true
		}
	}
	return "", "", false
}
