package common

import (
	"crypto/sha256"
	"strings"
)

// anthropicIDAlphabet is base62 with the visually ambiguous characters
// I, O and l removed, leaving 59 characters. This matches the alphabet
// reverse-engineered from 30 real api.anthropic.com request-id / message-id
// samples (research §4): no I/O/l ever appears across 30×24 = 720 characters.
const anthropicIDAlphabet = "0123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

const anthropicIDBase = 59 // len(anthropicIDAlphabet)

// bedrockMsgIDAlphabet is lowercase base36 (0-9a-z). Real AWS Bedrock
// message ids observed in the wild use only these characters after the
// "msg_bdrk_" prefix.
const bedrockMsgIDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// MessageIDStyleAnthropic / MessageIDStyleBedrock select the client-facing
// message.id profile used by EncodeAnthropicMessageIDStyle. Unknown styles
// fall back to anthropic.
const (
	MessageIDStyleAnthropic = "anthropic"
	MessageIDStyleBedrock   = "bedrock"
)

// anthropicReqIDTimeWidth is the fixed width (in base59 chars) of the
// time-ordered prefix in a generated request id. 59^7 ≈ 2.0e12 comfortably
// covers Unix-second timestamps far past the year 2100, so a fixed width of 7
// keeps the prefix monotonically increasing (left-padded) and therefore
// lexicographically sortable, mirroring the KSUID-style ordering of real
// Anthropic request ids.
const anthropicReqIDTimeWidth = 7

// anthropicReqIDRandWidth is the number of base59 chars taken from the hash of
// the internal id. 7 (time) + 15 (hash) = 22, which combined with the leading
// "01" format marker yields the 24-character suffix observed on real ids.
const anthropicReqIDRandWidth = 15

// anthropicMsgIDWidth is the number of base59 chars taken from the hash of the
// upstream message id. 2 ("01" marker) + 22 = 24, matching real msg_ ids.
const anthropicMsgIDWidth = 22

// bedrockMsgIDWidth is the number of base36-lowercase chars after the
// "msg_bdrk_" prefix. Real Bedrock samples are consistently 50 chars, for a
// total id length of len("msg_bdrk_")+50 = 59.
const bedrockMsgIDWidth = 50

// encodeBase59FixedWidth encodes a non-negative integer into base59 using
// anthropicIDAlphabet, left-padded with the zero-digit ('0') to exactly width
// characters. Most-significant digit first, so lexical order == numeric order.
func encodeBase59FixedWidth(n uint64, width int) string {
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = anthropicIDAlphabet[n%anthropicIDBase]
		n /= anthropicIDBase
	}
	return string(buf)
}

// hashToAlphabet maps a string deterministically to a string of exactly width
// characters drawn from alphabet by interpreting consecutive bytes of its
// SHA-256 digest as alphabet digits. Same input always yields the same output.
// alphabet must be non-empty; width must be > 0 (callers own those invariants).
func hashToAlphabet(input string, width int, alphabet string) string {
	sum := sha256.Sum256([]byte(input))
	base := len(alphabet)
	buf := make([]byte, width)
	for i := 0; i < width; i++ {
		buf[i] = alphabet[int(sum[i%len(sum)])%base]
	}
	return string(buf)
}

// hashToBase59 maps a string deterministically to a base59 string of exactly
// width characters. Thin wrapper over hashToAlphabet for the Anthropic alphabet.
func hashToBase59(input string, width int) string {
	return hashToAlphabet(input, width, anthropicIDAlphabet)
}

// NormalizeMessageIDStyle trims and lowercases style, mapping "" / "anthropic"
// and any unknown value to MessageIDStyleAnthropic, and "bedrock" to
// MessageIDStyleBedrock. Safe for operator-supplied config.
func NormalizeMessageIDStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case MessageIDStyleBedrock:
		return MessageIDStyleBedrock
	default:
		return MessageIDStyleAnthropic
	}
}

// EncodeAnthropicRequestID deterministically re-encodes an internal request id
// into an Anthropic-style request id: "req_01" + base59(timestamp, 7) +
// base59(SHA-256(internalID), 15), for a total of "req_" + 24 characters.
//
// The result is:
//   - deterministic — same (internalID, unixSec) always produces the same id,
//     so the server log line "request-id=req_... internal=<internalID>" lets an
//     operator reverse-map a client-facing id back to the internal one;
//   - time-ordered — the fixed-width timestamp prefix sorts lexicographically
//     in chronological order, like real Anthropic request ids (KSUID style);
//   - format-compatible — leading "01" marker and the 59-char I/O/l-free
//     alphabet match the reverse-engineered official format (research §4).
func EncodeAnthropicRequestID(internalID string, unixSec int64) string {
	var ts uint64
	if unixSec > 0 {
		ts = uint64(unixSec)
	}
	var b strings.Builder
	b.WriteString("req_01")
	b.WriteString(encodeBase59FixedWidth(ts, anthropicReqIDTimeWidth))
	b.WriteString(hashToBase59(internalID, anthropicReqIDRandWidth))
	return b.String()
}

// EncodeAnthropicMessageID deterministically re-encodes an upstream message id
// into the default Anthropic-style message id profile (style=anthropic):
// "msg_01" + base59(SHA-256(upstreamID), 22), total "msg_" + 24 characters.
// Prefer EncodeAnthropicMessageIDStyle when the deployment may select bedrock.
func EncodeAnthropicMessageID(upstreamID string) string {
	return EncodeAnthropicMessageIDStyle(upstreamID, MessageIDStyleAnthropic)
}

// EncodeAnthropicMessageIDStyle deterministically re-encodes an upstream
// message id into a client-facing message id using the given style profile:
//
//	anthropic (default / unknown): "msg_01"  + base59(SHA-256, 22)          → len 28
//	bedrock:                       "msg_bdrk_" + base36lower(SHA-256, 50) → len 59
//
// Profiles bind prefix, alphabet, and suffix width together so operators
// cannot produce a four-neither-nor id by swapping only the prefix.
// Determinism lets the upstream id be reverse-verified from logs.
func EncodeAnthropicMessageIDStyle(upstreamID, style string) string {
	switch NormalizeMessageIDStyle(style) {
	case MessageIDStyleBedrock:
		return "msg_bdrk_" + hashToAlphabet(upstreamID, bedrockMsgIDWidth, bedrockMsgIDAlphabet)
	default:
		return "msg_01" + hashToBase59(upstreamID, anthropicMsgIDWidth)
	}
}
