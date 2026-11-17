// Git pkt-line framing: a 4-hex-digit length prefix (counting itself),
// 0000 flush, 0001 delim, 0002 response-end, and raw trailing bytes
// (a pack follows the flush in receive-pack bodies).
package main

import (
	"fmt"
)

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")
var pktResponseEnd = []byte("0002")

func pktLine(data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(data)+4))...)
	return append(out, data...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

// pktLines encodes a sequence of data packets and flush/delim markers.
func pktLines(items []any) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, pktFlush...)
			} else if v == "delim" {
				out = append(out, pktDelim...)
			} else if v == "response-end" {
				out = append(out, pktResponseEnd...)
			} else {
				out = append(out, pktLineStr(v)...)
			}
		case []byte:
			out = append(out, pktLine(v)...)
		}
	}
	return out
}

type Packet struct {
	Kind    string // data | flush | delim | response-end
	Payload []byte
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) int {
	if c <= '9' {
		return int(c - '0')
	}
	return int(c|32) - 87
}

// decodePackets splits a buffer into packets plus any trailing raw bytes.
func decodePackets(buf []byte) (packets []Packet, trailing []byte) {
	offset := 0
	for offset+4 <= len(buf) {
		h0, h1, h2, h3 := buf[offset], buf[offset+1], buf[offset+2], buf[offset+3]
		if !isHexByte(h0) || !isHexByte(h1) || !isHexByte(h2) || !isHexByte(h3) {
			break
		}
		length := (hexVal(h0) << 12) | (hexVal(h1) << 8) | (hexVal(h2) << 4) | hexVal(h3)
		switch {
		case length == 0:
			packets = append(packets, Packet{Kind: "flush"})
			offset += 4
		case length == 1:
			packets = append(packets, Packet{Kind: "delim"})
			offset += 4
		case length == 2:
			packets = append(packets, Packet{Kind: "response-end"})
			offset += 4
		default:
			if length < 4 || offset+length > len(buf) {
				return packets, buf[offset:]
			}
			packets = append(packets, Packet{Kind: "data", Payload: buf[offset+4 : offset+length]})
			offset += length
		}
	}
	return packets, buf[offset:]
}

// packetLines returns the data-packet payloads as strings, control packets
// dropped.
func packetLines(packets []Packet) []string {
	var out []string
	for _, p := range packets {
		if p.Kind == "data" {
			out = append(out, string(p.Payload))
		}
	}
	return out
}

// stripTrailingNewline removes one trailing \r?\n, as the upload-pack
// parser does before comparing commands.
func stripTrailingNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
		if len(s) > 0 && s[len(s)-1] == '\r' {
			s = s[:len(s)-1]
		}
	}
	return s
}
