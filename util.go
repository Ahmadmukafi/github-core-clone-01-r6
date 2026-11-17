// Shared helpers: the deterministic clock, hashes, encodings, and the
// JavaScript-flavoured parsing the forge's wire behaviour depends on.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The scenario viewer identity (the kit's seed is fixed).
const viewerLogin = "adaiversen"
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const viewerID = 1000

const zeroOid = "0000000000000000000000000000000000000000"

// nextRequestId is the util request-id scheme (req-<seq36><hash36>).
var utilRequestSeq int64

func nextRequestId() string {
	seq := atomic.AddInt64(&utilRequestSeq, 1)
	return fmt.Sprintf("req-%s%s", strconv.FormatInt(seq, 36), strconv.FormatInt(seq*2654435761%46656, 36))
}

// The forge's clock never reads the wall clock: every timestamp comes from a
// counter anchored at 2025-01-01T00:00:00Z, one second per event. Two runs
// fed the same request sequence therefore produce identical timestamps.
const epochSec = 1735689600

var tickCount int64

func tick() int64 {
	return epochSec + atomic.AddInt64(&tickCount, 1)
}

// isoOf renders a Unix second as an ISO-8601 UTC string with no millis.
func isoOf(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// hashObject is git's object id: sha1("<type> <len>\0" + payload).
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOid(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// --- base64, with the lenient decoder the reference runtime uses ---

func b64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func b64EncodeURL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func cleanB64(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			// dropped
		case c == '-':
			b.WriteByte('+')
		case c == '_':
			b.WriteByte('/')
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=':
			b.WriteByte(c)
		default:
			// other characters are ignored by the lenient decoder
		}
	}
	return b.String()
}

func b64Decode(s string) []byte {
	clean := cleanB64(s)
	if clean == "" {
		return []byte{}
	}
	for len(clean)%4 != 0 {
		clean += "="
	}
	out, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		// Fall back to raw decoding without padding.
		out2, err2 := base64.RawStdEncoding.DecodeString(strings.TrimRight(clean, "="))
		if err2 != nil {
			return []byte{}
		}
		return out2
	}
	return out
}

func b64DecodeURL(s string) []byte { return b64Decode(s) }

// globalId encodes a typed identity ("Issue:123" style) as base64url.
func globalId(parts ...string) string {
	return b64EncodeURL([]byte(strings.Join(parts, ":")))
}

func parseGlobalId(id string) (kind string, parts []string, ok bool) {
	raw := b64DecodeURL(id)
	if len(raw) == 0 {
		return "", nil, false
	}
	segs := strings.Split(string(raw), ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

// jsParseInt mirrors Number.parseInt(s, 10): leading whitespace, optional
// sign, then as many decimal digits as follow; no digits means NaN.
func jsParseInt(s string) (int64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	digits := s[start:i]
	var n int64
	for _, c := range digits {
		n = n*10 + int64(c-'0')
		if n > math.MaxInt64/10 {
			// values this large never appear in the harness; saturate
			n = math.MaxInt64
			break
		}
	}
	if neg {
		n = -n
	}
	return n, true
}

// jsStringify mirrors String(x): undefined -> "undefined", null -> "null",
// arrays join with commas (null/undefined elements become empty strings),
// objects -> "[object Object]".
func jsStringify(v *JVal) string {
	if v == nil {
		return "undefined"
	}
	switch v.Kind {
	case JUndef:
		return "undefined"
	case JNull:
		return "null"
	case JNum:
		if math.IsNaN(v.Num) {
			return "NaN"
		}
		if math.IsInf(v.Num, 1) {
			return "Infinity"
		}
		if math.IsInf(v.Num, -1) {
			return "-Infinity"
		}
		return jsNum(v.Num)
	case JStr:
		return v.Str
	case JBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case JArr:
		parts := make([]string, len(v.Arr))
		for i, item := range v.Arr {
			if item == nil || item.Kind == JUndef || item.Kind == JNull {
				parts[i] = ""
			} else {
				parts[i] = jsStringify(item)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// jsNumber mirrors Number(x): NaN when the value cannot be converted.
func jsNumber(v *JVal) (float64, bool) {
	if v == nil {
		return math.NaN(), false
	}
	switch v.Kind {
	case JUndef, JObj, JObjRef, JArrRef:
		return math.NaN(), false
	case JNull:
		return 0, true
	case JNum:
		return v.Num, true
	case JBool:
		if v.Bool {
			return 1, true
		}
		return 0, true
	case JStr:
		return jsNumberString(v.Str)
	case JArr:
		return jsNumberString(jsStringify(v))
	default:
		return math.NaN(), false
	}
}

func jsNumberString(s string) (float64, bool) {
	t := strings.Trim(s, " \t\n\r\f\v")
	if t == "" {
		return 0, true
	}
	if t == "Infinity" || t == "+Infinity" {
		return math.Inf(1), true
	}
	if t == "-Infinity" {
		return math.Inf(-1), true
	}
	// Hex literals.
	neg := false
	rest := t
	if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "-") {
		neg = rest[0] == '-'
		rest = rest[1:]
	}
	if len(rest) > 2 && rest[0] == '0' && (rest[1] == 'x' || rest[1] == 'X') {
		n, err := strconv.ParseInt(rest[2:], 16, 64)
		if err != nil {
			return math.NaN(), false
		}
		if neg {
			n = -n
		}
		return float64(n), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return math.NaN(), false
	}
	return f, true
}
