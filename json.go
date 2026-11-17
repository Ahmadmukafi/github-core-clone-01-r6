// JSON values with JavaScript semantics: ordered object members, no HTML
// escaping, undefined (omitted in objects, null in arrays), and JS number
// formatting. The forge's responses must be byte-identical to the reference
// implementation's JSON.stringify output, so the standard library encoder
// (sorted map keys, HTML escaping) cannot be used here.
package main

import (
	"math"
	"strconv"
	"strings"
)

type JKind int

const (
	JNull JKind = iota
	JBool
	JNum
	JStr
	JArr
	JObj
	JUndef
	JRaw // pre-serialized JSON bytes, embedded verbatim
	JObjRef
	JArrRef
)

type JMember struct {
	Key string
	Val *JVal
}

type JVal struct {
	Kind   JKind
	Bool   bool
	Num    float64
	Str    string
	Arr    []*JVal
	Obj    []JMember
	Raw    []byte
	ObjRef *GqlObject
	ArrRef []*GqlObject
}

func JNullVal() *JVal             { return &JVal{Kind: JNull} }
func JBoolVal(b bool) *JVal       { return &JVal{Kind: JBool, Bool: b} }
func JNumVal(n float64) *JVal     { return &JVal{Kind: JNum, Num: n} }
func JIntVal(n int64) *JVal       { return &JVal{Kind: JNum, Num: float64(n)} }
func JStrVal(s string) *JVal      { return &JVal{Kind: JStr, Str: s} }
func JArrVal(items ...*JVal) *JVal { return &JVal{Kind: JArr, Arr: items} }
func JUndefVal() *JVal            { return &JVal{Kind: JUndef} }
func JRawVal(b []byte) *JVal      { return &JVal{Kind: JRaw, Raw: b} }

func JObjVal(members ...JMember) *JVal { return &JVal{Kind: JObj, Obj: members} }

func JObjRefVal(obj *GqlObject) *JVal { return &JVal{Kind: JObjRef, ObjRef: obj} }

func JArrRefVal(objs []*GqlObject) *JVal { return &JVal{Kind: JArrRef, ArrRef: objs} }

// ObjSet sets key (replacing in place, keeping first position) like a JS object.
func (v *JVal) ObjSet(key string, val *JVal) {
	for i := range v.Obj {
		if v.Obj[i].Key == key {
			v.Obj[i].Val = val
			return
		}
	}
	v.Obj = append(v.Obj, JMember{Key: key, Val: val})
}

func (v *JVal) ObjGet(key string) *JVal {
	for i := range v.Obj {
		if v.Obj[i].Key == key {
			return v.Obj[i].Val
		}
	}
	return nil
}

func (v *JVal) IsObject() bool { return v != nil && v.Kind == JObj }
func (v *JVal) IsArray() bool  { return v != nil && v.Kind == JArr }

// AsString returns the string value, or "" for anything else (JS String(x) is
// NOT used here; callers that need String(undefined)==="undefined" handle it).
func (v *JVal) AsString() string {
	if v != nil && v.Kind == JStr {
		return v.Str
	}
	return ""
}

func (v *JVal) AsFloat() (float64, bool) {
	if v != nil && v.Kind == JNum {
		return v.Num, true
	}
	return 0, false
}

func (v *JVal) AsBool() bool {
	return v != nil && v.Kind == JBool && v.Bool
}

// jsNum formats a float64 the way JSON.stringify does: integers without a
// decimal point, otherwise the shortest round-trip decimal, with exponential
// notation for very large/small magnitudes. NaN and infinities become null.
func jsNum(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	if f == float64(int64(f)) && f > -1e21 && f < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// Go prints 1e-07; JS prints 1e-7. Normalize the exponent.
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		exp := s[i+1:]
		neg := false
		if strings.HasPrefix(exp, "-") || strings.HasPrefix(exp, "+") {
			neg = exp[0] == '-'
			exp = exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		if neg {
			exp = "-" + exp
		}
		s = s[:i] + "e" + exp
	}
	return s
}

var hexDigits = "0123456789abcdef"

func jsString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		default:
			if c < 0x20 {
				b.WriteString("\\u00")
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Marshal renders the value exactly as JSON.stringify would.
func (v *JVal) Marshal() []byte {
	var b strings.Builder
	v.marshalTo(&b)
	return []byte(b.String())
}

func (v *JVal) marshalTo(b *strings.Builder) {
	switch v.Kind {
	case JNull, JUndef:
		b.WriteString("null")
	case JBool:
		if v.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case JNum:
		b.WriteString(jsNum(v.Num))
	case JStr:
		b.WriteString(jsString(v.Str))
	case JRaw:
		b.Write(v.Raw)
	case JObjRef, JArrRef:
		// Executor references must be resolved before serialization; if one
		// leaks through, emit null rather than crashing the response.
		b.WriteString("null")
	case JArr:
		b.WriteByte('[')
		for i, item := range v.Arr {
			if i > 0 {
				b.WriteByte(',')
			}
			if item.Kind == JUndef {
				b.WriteString("null")
			} else {
				item.marshalTo(b)
			}
		}
		b.WriteByte(']')
	case JObj:
		b.WriteByte('{')
		first := true
		for _, m := range v.Obj {
			if m.Val.Kind == JUndef {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(jsString(m.Key))
			b.WriteByte(':')
			m.Val.marshalTo(b)
		}
		b.WriteByte('}')
	}
}

// --- parser ---

type jParser struct {
	data []byte
	pos  int
}

func ParseJSON(data []byte) (*JVal, error) {
	p := &jParser{data: data}
	p.skipWS()
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, errSyntax
	}
	return v, nil
}

var errSyntax = &jsonError{"syntax error"}

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }

func (p *jParser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jParser) parseValue() (*JVal, error) {
	if p.pos >= len(p.data) {
		return nil, errSyntax
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return JStrVal(s), nil
	case c == 't':
		if p.lit("true") {
			return JBoolVal(true), nil
		}
	case c == 'f':
		if p.lit("false") {
			return JBoolVal(false), nil
		}
	case c == 'n':
		if p.lit("null") {
			return JNullVal(), nil
		}
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	}
	return nil, errSyntax
}

func (p *jParser) lit(s string) bool {
	if p.pos+len(s) <= len(p.data) && string(p.data[p.pos:p.pos+len(s)]) == s {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *jParser) parseObject() (*JVal, error) {
	p.pos++ // {
	obj := &JVal{Kind: JObj}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, errSyntax
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, errSyntax
		}
		p.pos++
		p.skipWS()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj.ObjSet(key, val)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, errSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, errSyntax
		}
	}
}

func (p *jParser) parseArray() (*JVal, error) {
	p.pos++ // [
	arr := &JVal{Kind: JArr}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		p.skipWS()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr.Arr = append(arr.Arr, val)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, errSyntax
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, errSyntax
		}
	}
}

func (p *jParser) parseString() (string, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.data) {
				return "", errSyntax
			}
			esc := p.data[p.pos]
			p.pos++
			switch esc {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if p.pos+4 > len(p.data) {
					return "", errSyntax
				}
				cp, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
				if err != nil {
					return "", errSyntax
				}
				p.pos += 4
				r := rune(cp)
				if r >= 0xD800 && r <= 0xDBFF && p.pos+6 <= len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
					lo, err := strconv.ParseUint(string(p.data[p.pos+2:p.pos+6]), 16, 32)
					if err == nil && lo >= 0xDC00 && lo <= 0xDFFF {
						r = 0x10000 + ((r - 0xD800) << 10) + (rune(lo) - 0xDC00)
						p.pos += 6
					}
				}
				b.WriteRune(r)
			default:
				return "", errSyntax
			}
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", errSyntax
}

func (p *jParser) parseNumber() (*JVal, error) {
	start := p.pos
	if p.pos < len(p.data) && p.data[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	f, err := strconv.ParseFloat(string(p.data[start:p.pos]), 64)
	if err != nil {
		return nil, errSyntax
	}
	return JNumVal(f), nil
}
