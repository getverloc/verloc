package server

import (
	"bytes"
	"encoding/json"
	"strconv"
)

const maxJSONNestingDepth = 10000

// byteSpan locates a JSON value slice [start, end) inside a raw request body.
type byteSpan struct {
	start int
	end   int
	set   bool
}

// topLevelScan captures byte offsets discovered during the single top-level
// JSON scan in parseRequestMetadata so downstream transforms (model splice,
// max-token capping) can rewrite the body without rescanning it.
type topLevelScan struct {
	valid       bool
	closeBrace  int
	memberCount int
	model       byteSpan
	capSpans    [3]byteSpan // max_tokens, max_completion_tokens, max_output_tokens
	capVals     [3]uint64
}

func skipJSONWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func isJSONHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// scanJSONString scans a JSON string starting at b[i] == '"', returning the
// byte index immediately after the closing quote, whether any backslash escape
// or non-ASCII byte was present, and whether the string syntax was valid.
func scanJSONString(b []byte, i int) (end int, hasEscape bool, hasNonASCII bool, ok bool) {
	if i >= len(b) || b[i] != '"' {
		return 0, false, false, false
	}
	i++
	for i < len(b) {
		c := b[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			if c >= 0x80 {
				hasNonASCII = true
			}
			i++
			continue
		}
		if c == '"' {
			return i + 1, hasEscape, hasNonASCII, true
		}
		if c < 0x20 {
			return 0, hasEscape, hasNonASCII, false
		}
		// c == '\\'
		hasEscape = true
		i++
		if i >= len(b) {
			return 0, true, hasNonASCII, false
		}
		switch b[i] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i++
		case 'u':
			if i+4 >= len(b) || !isJSONHex(b[i+1]) || !isJSONHex(b[i+2]) || !isJSONHex(b[i+3]) || !isJSONHex(b[i+4]) {
				return 0, true, hasNonASCII, false
			}
			i += 5
		default:
			return 0, true, hasNonASCII, false
		}
	}
	return 0, hasEscape, hasNonASCII, false
}

func scanJSONNumber(b []byte, i int) (int, bool) {
	n := len(b)
	if i < n && b[i] == '-' {
		i++
	}
	if i >= n {
		return 0, false
	}
	if b[i] == '0' {
		i++
	} else if b[i] >= '1' && b[i] <= '9' {
		i++
		for i < n && b[i] >= '0' && b[i] <= '9' {
			i++
		}
	} else {
		return 0, false
	}
	if i < n && b[i] == '.' {
		i++
		if i >= n || b[i] < '0' || b[i] > '9' {
			return 0, false
		}
		for i < n && b[i] >= '0' && b[i] <= '9' {
			i++
		}
	}
	if i < n && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < n && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= n || b[i] < '0' || b[i] > '9' {
			return 0, false
		}
		for i < n && b[i] >= '0' && b[i] <= '9' {
			i++
		}
	}
	return i, true
}

func scanJSONValue(b []byte, i int, depth int) (int, bool) {
	if i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case '"':
		end, _, _, ok := scanJSONString(b, i)
		return end, ok
	case '{':
		depth++
		if depth > maxJSONNestingDepth {
			return 0, false
		}
		i = skipJSONWS(b, i+1)
		if i < len(b) && b[i] == '}' {
			return i + 1, true
		}
		for {
			end, _, _, ok := scanJSONString(b, i)
			if !ok {
				return 0, false
			}
			i = skipJSONWS(b, end)
			if i >= len(b) || b[i] != ':' {
				return 0, false
			}
			i = skipJSONWS(b, i+1)
			i, ok = scanJSONValue(b, i, depth)
			if !ok {
				return 0, false
			}
			i = skipJSONWS(b, i)
			if i >= len(b) {
				return 0, false
			}
			if b[i] == '}' {
				return i + 1, true
			}
			if b[i] != ',' {
				return 0, false
			}
			i = skipJSONWS(b, i+1)
		}
	case '[':
		depth++
		if depth > maxJSONNestingDepth {
			return 0, false
		}
		i = skipJSONWS(b, i+1)
		if i < len(b) && b[i] == ']' {
			return i + 1, true
		}
		for {
			var ok bool
			i, ok = scanJSONValue(b, i, depth)
			if !ok {
				return 0, false
			}
			i = skipJSONWS(b, i)
			if i >= len(b) {
				return 0, false
			}
			if b[i] == ']' {
				return i + 1, true
			}
			if b[i] != ',' {
				return 0, false
			}
			i = skipJSONWS(b, i+1)
		}
	case 't':
		if i+4 <= len(b) && b[i+1] == 'r' && b[i+2] == 'u' && b[i+3] == 'e' {
			return i + 4, true
		}
		return 0, false
	case 'f':
		if i+5 <= len(b) && b[i+1] == 'a' && b[i+2] == 'l' && b[i+3] == 's' && b[i+4] == 'e' {
			return i + 5, true
		}
		return 0, false
	case 'n':
		if i+4 <= len(b) && b[i+1] == 'u' && b[i+2] == 'l' && b[i+3] == 'l' {
			return i + 4, true
		}
		return 0, false
	default:
		if b[i] == '-' || (b[i] >= '0' && b[i] <= '9') {
			return scanJSONNumber(b, i)
		}
		return 0, false
	}
}

// validateJSON is a fast, zero-allocation RFC 8259 validator equivalent to
// encoding/json.Valid without indirect state-machine calls per byte.
func validateJSON(b []byte) bool {
	i := skipJSONWS(b, 0)
	if i >= len(b) {
		return false
	}
	end, ok := scanJSONValue(b, i, 0)
	if !ok {
		return false
	}
	return skipJSONWS(b, end) == len(b)
}

func parsePureUint64(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if v > (^uint64(0)-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	return v, true
}

func equalFoldASCII(b []byte, target string) bool {
	if len(b) != len(target) {
		return false
	}
	for i := range len(b) {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != target[i] {
			return false
		}
	}
	return true
}

// appendJSONString encodes s as a JSON string identical to encoding/json.Marshal.
// For ordinary ASCII strings without special characters it writes directly into
// dst with zero allocations; otherwise it falls back to encoding/json.Marshal.
func appendJSONString(dst []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if c < 0x20 || c >= 0x80 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			encoded, err := json.Marshal(s)
			if err != nil {
				return dst
			}
			return append(dst, encoded...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	dst = append(dst, '"')
	return dst
}

// parseRequestMetadata validates raw JSON and extracts requestMetadata in a
// single byte-level pass while recording top-level byte spans for zero-rescan
// downstream transforms. Any edge case (escaped or case-folded keys, duplicate
// tracked keys, non-integer numbers, non-object JSON, syntax errors) falls back
// to encoding/json.Unmarshal so accept/reject behavior is identical.
func parseRequestMetadata(raw []byte) (requestMetadata, topLevelScan, error) {
	var md requestMetadata
	var scan topLevelScan
	i := skipJSONWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return md, scan, json.Unmarshal(raw, &md)
	}
	i = skipJSONWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		if skipJSONWS(raw, i+1) == len(raw) {
			scan.valid = true
			scan.closeBrace = i
			return md, scan, nil
		}
		return md, scan, json.Unmarshal(raw, &md)
	}
	for {
		kStart := i
		kEnd, kEsc, _, ok := scanJSONString(raw, i)
		if !ok || kEsc {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		key := raw[kStart+1 : kEnd-1]
		i = skipJSONWS(raw, kEnd)
		if i >= len(raw) || raw[i] != ':' {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		valStart := skipJSONWS(raw, i+1)
		if valStart >= len(raw) {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		var valEnd int
		var valEsc, valNonASCII bool
		if raw[valStart] == '"' {
			valEnd, valEsc, valNonASCII, ok = scanJSONString(raw, valStart)
		} else {
			valEnd, ok = scanJSONValue(raw, valStart, 1)
		}
		if !ok {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		scan.memberCount++
		val := raw[valStart:valEnd]
		switch len(key) {
		case 5:
			if string(key) == "model" {
				if scan.model.set {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
				scan.model = byteSpan{start: valStart, end: valEnd, set: true}
				if raw[valStart] == '"' {
					if valEsc || valNonASCII {
						if err := json.Unmarshal(val, &md.Model); err != nil {
							return md, topLevelScan{}, json.Unmarshal(raw, &md)
						}
					} else {
						md.Model = string(raw[valStart+1 : valEnd-1])
					}
				} else if !bytes.Equal(val, jsonNullBytes) {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
			} else if equalFoldASCII(key, "model") {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
		case 6:
			if string(key) == "stream" {
				switch {
				case bytes.Equal(val, jsonTrueBytes):
					md.Stream = true
				case bytes.Equal(val, jsonFalseBytes):
					md.Stream = false
				case bytes.Equal(val, jsonNullBytes):
				default:
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
			} else if equalFoldASCII(key, "stream") {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
		case 10:
			if string(key) == "max_tokens" {
				if scan.capSpans[0].set {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
				if bytes.Equal(val, jsonNullBytes) {
					scan.capSpans[0] = byteSpan{start: valStart, end: valEnd, set: true}
				} else if v, ok := parsePureUint64(val); ok {
					md.MaxTokens = v
					scan.capSpans[0] = byteSpan{start: valStart, end: valEnd, set: true}
					scan.capVals[0] = v
				} else {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
			} else if equalFoldASCII(key, "max_tokens") {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
		case 17:
			if string(key) == "max_output_tokens" {
				if scan.capSpans[2].set {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
				if bytes.Equal(val, jsonNullBytes) {
					scan.capSpans[2] = byteSpan{start: valStart, end: valEnd, set: true}
				} else if v, ok := parsePureUint64(val); ok {
					md.MaxOutput = v
					scan.capSpans[2] = byteSpan{start: valStart, end: valEnd, set: true}
					scan.capVals[2] = v
				} else {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
			} else if equalFoldASCII(key, "max_output_tokens") {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
		case 21:
			if string(key) == "max_completion_tokens" {
				if scan.capSpans[1].set {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
				if bytes.Equal(val, jsonNullBytes) {
					scan.capSpans[1] = byteSpan{start: valStart, end: valEnd, set: true}
				} else if v, ok := parsePureUint64(val); ok {
					md.MaxCompletion = v
					scan.capSpans[1] = byteSpan{start: valStart, end: valEnd, set: true}
					scan.capVals[1] = v
				} else {
					return md, topLevelScan{}, json.Unmarshal(raw, &md)
				}
			} else if equalFoldASCII(key, "max_completion_tokens") {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
		}
		i = skipJSONWS(raw, valEnd)
		if i >= len(raw) {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		if raw[i] == '}' {
			if skipJSONWS(raw, i+1) != len(raw) {
				return md, topLevelScan{}, json.Unmarshal(raw, &md)
			}
			scan.valid = true
			scan.closeBrace = i
			return md, scan, nil
		}
		if raw[i] != ',' {
			return md, topLevelScan{}, json.Unmarshal(raw, &md)
		}
		i = skipJSONWS(raw, i+1)
	}
}

var (
	jsonNullBytes  = []byte("null")
	jsonTrueBytes  = []byte("true")
	jsonFalseBytes = []byte("false")
)

type byteEdit struct {
	start int
	end   int
	repl  []byte
}

// applyByteEdits splices non-overlapping edits (in any order) into raw in a
// single allocation.
func applyByteEdits(raw []byte, edits []byteEdit) []byte {
	// Insertion sort — edits has at most 4 elements.
	for i := 1; i < len(edits); i++ {
		e := edits[i]
		j := i - 1
		for j >= 0 && edits[j].start > e.start {
			edits[j+1] = edits[j]
			j--
		}
		edits[j+1] = e
	}
	newLen := len(raw)
	for _, e := range edits {
		newLen += len(e.repl) - (e.end - e.start)
	}
	out := make([]byte, 0, newLen)
	pos := 0
	for _, e := range edits {
		out = append(out, raw[pos:e.start]...)
		out = append(out, e.repl...)
		pos = e.end
	}
	out = append(out, raw[pos:]...)
	return out
}

// spliceFromScan rewrites top-level "model" (and caps output-token fields when
// limit > 0) using byte spans already discovered by parseRequestMetadata.
func spliceFromScan(raw []byte, scan topLevelScan, upstreamModel string, limit int) []byte {
	var modelBuf [128]byte
	modelJSON := appendJSONString(modelBuf[:0], upstreamModel)

	var insertBuf [140]byte
	var limitBytes []byte

	var editsBuf [4]byteEdit
	edits := editsBuf[:0]

	if scan.model.set {
		if !bytes.Equal(raw[scan.model.start:scan.model.end], modelJSON) {
			edits = append(edits, byteEdit{start: scan.model.start, end: scan.model.end, repl: modelJSON})
		}
	} else {
		ins := insertBuf[:0]
		if scan.memberCount > 0 {
			ins = append(ins, ',')
		}
		ins = append(ins, `"model":`...)
		ins = append(ins, modelJSON...)
		edits = append(edits, byteEdit{start: scan.closeBrace, end: scan.closeBrace, repl: ins})
	}

	if limit > 0 {
		uLimit := uint64(limit)
		for idx := range scan.capSpans {
			sp := scan.capSpans[idx]
			if sp.set && scan.capVals[idx] > uLimit {
				if limitBytes == nil {
					limitBytes = strconv.AppendInt(nil, int64(limit), 10)
				}
				edits = append(edits, byteEdit{start: sp.start, end: sp.end, repl: limitBytes})
			}
		}
	}

	if len(edits) == 0 {
		return raw
	}
	return applyByteEdits(raw, edits)
}

// spliceTopLevelModel rewrites or inserts the top-level "model" JSON field in
// place without decoding nested structures. Returns (result, handled):
//   - handled == true: result is final (either spliced body or original raw on
//     syntax error / non-object JSON).
//   - handled == false: edge case (escaped key, duplicate "model" key) — caller
//     should fall back to encoding/json.
func spliceTopLevelModel(raw []byte, upstreamModel string) ([]byte, bool) {
	i := skipJSONWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		// Non-object or empty JSON: preserve remapBody's contract of returning raw.
		return raw, true
	}
	i = skipJSONWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		if skipJSONWS(raw, i+1) != len(raw) {
			return raw, true
		}
		var buf [140]byte
		out := append(buf[:0], `{"model":`...)
		out = appendJSONString(out, upstreamModel)
		out = append(out, '}')
		res := make([]byte, len(out))
		copy(res, out)
		return res, true
	}
	var modelSpan byteSpan
	memberCount := 0
	for {
		kStart := i
		kEnd, kEsc, _, ok := scanJSONString(raw, i)
		if !ok {
			return raw, true
		}
		if kEsc {
			return nil, false
		}
		key := raw[kStart+1 : kEnd-1]
		i = skipJSONWS(raw, kEnd)
		if i >= len(raw) || raw[i] != ':' {
			return raw, true
		}
		valStart := skipJSONWS(raw, i+1)
		valEnd, ok := scanJSONValue(raw, valStart, 1)
		if !ok {
			return raw, true
		}
		memberCount++
		if len(key) == 5 && string(key) == "model" {
			if modelSpan.set {
				return nil, false // duplicate "model" key: let encoding/json canonicalize
			}
			modelSpan = byteSpan{start: valStart, end: valEnd, set: true}
		}
		i = skipJSONWS(raw, valEnd)
		if i >= len(raw) {
			return raw, true
		}
		if raw[i] == '}' {
			if skipJSONWS(raw, i+1) != len(raw) {
				return raw, true
			}
			var modelBuf [128]byte
			modelJSON := appendJSONString(modelBuf[:0], upstreamModel)
			if modelSpan.set {
				if bytes.Equal(raw[modelSpan.start:modelSpan.end], modelJSON) {
					return raw, true
				}
				out := make([]byte, 0, len(raw)-(modelSpan.end-modelSpan.start)+len(modelJSON))
				out = append(out, raw[:modelSpan.start]...)
				out = append(out, modelJSON...)
				out = append(out, raw[modelSpan.end:]...)
				return out, true
			}
			// Missing "model": insert before closing brace.
			out := make([]byte, 0, len(raw)+len(`,"model":`)+len(modelJSON))
			out = append(out, raw[:i]...)
			if memberCount > 0 {
				out = append(out, ',')
			}
			out = append(out, `"model":`...)
			out = append(out, modelJSON...)
			out = append(out, raw[i:]...)
			return out, true
		}
		if raw[i] != ',' {
			return raw, true
		}
		i = skipJSONWS(raw, i+1)
	}
}

// capMaxTokensFast scans top-level output-limit fields without decoding nested
// trees. Returns (result, handled); handled == false triggers the full decoder
// fallback for escaped/duplicate keys or floating-point values.
func capMaxTokensFast(raw []byte, limit int) ([]byte, bool) {
	i := skipJSONWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return raw, true
	}
	i = skipJSONWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return raw, true
	}
	uLimit := uint64(limit)
	var seen [3]bool
	var editsBuf [3]byteEdit
	edits := editsBuf[:0]
	var limitBytes []byte

	for {
		kStart := i
		kEnd, kEsc, _, ok := scanJSONString(raw, i)
		if !ok {
			return raw, true
		}
		if kEsc {
			return nil, false
		}
		key := raw[kStart+1 : kEnd-1]
		i = skipJSONWS(raw, kEnd)
		if i >= len(raw) || raw[i] != ':' {
			return raw, true
		}
		valStart := skipJSONWS(raw, i+1)
		valEnd, ok := scanJSONValue(raw, valStart, 1)
		if !ok {
			return raw, true
		}
		fieldIdx := -1
		switch len(key) {
		case 10:
			if string(key) == "max_tokens" {
				fieldIdx = 0
			}
		case 21:
			if string(key) == "max_completion_tokens" {
				fieldIdx = 1
			}
		case 17:
			if string(key) == "max_output_tokens" {
				fieldIdx = 2
			}
		}
		if fieldIdx >= 0 {
			if seen[fieldIdx] {
				return nil, false
			}
			seen[fieldIdx] = true
			val := raw[valStart:valEnd]
			if raw[valStart] >= '0' && raw[valStart] <= '9' {
				v, pure := parsePureUint64(val)
				if !pure {
					return nil, false // float or scientific notation
				}
				if v > uLimit {
					if limitBytes == nil {
						limitBytes = strconv.AppendInt(nil, int64(limit), 10)
					}
					edits = append(edits, byteEdit{start: valStart, end: valEnd, repl: limitBytes})
				}
			} else if raw[valStart] == '-' {
				// Negative number cannot exceed positive limit, no edit needed.
			}
		}
		i = skipJSONWS(raw, valEnd)
		if i >= len(raw) {
			return raw, true
		}
		if raw[i] == '}' {
			if skipJSONWS(raw, i+1) != len(raw) {
				return raw, true
			}
			if len(edits) == 0 {
				return raw, true
			}
			return applyByteEdits(raw, edits), true
		}
		if raw[i] != ',' {
			return raw, true
		}
		i = skipJSONWS(raw, i+1)
	}
}

// findTopLevelField returns the raw JSON value slice for a single top-level key
// inside a JSON object without decoding sibling fields. Returns (val, found, ok);
// ok == false indicates escaped/duplicate/case-folded keys or non-object syntax
// so callers can fall back to encoding/json.
func findTopLevelField(raw []byte, target string) ([]byte, bool, bool) {
	i := skipJSONWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false, false
	}
	i = skipJSONWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return nil, false, skipJSONWS(raw, i+1) == len(raw)
	}
	var match []byte
	found := false
	for {
		kStart := i
		kEnd, kEsc, _, ok := scanJSONString(raw, i)
		if !ok || kEsc {
			return nil, false, false
		}
		key := raw[kStart+1 : kEnd-1]
		i = skipJSONWS(raw, kEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, false, false
		}
		valStart := skipJSONWS(raw, i+1)
		valEnd, ok := scanJSONValue(raw, valStart, 1)
		if !ok {
			return nil, false, false
		}
		if len(key) == len(target) && string(key) == target {
			if found {
				return nil, false, false
			}
			match = raw[valStart:valEnd]
			found = true
		} else if equalFoldASCII(key, target) {
			return nil, false, false
		}
		i = skipJSONWS(raw, valEnd)
		if i >= len(raw) {
			return nil, false, false
		}
		if raw[i] == '}' {
			if skipJSONWS(raw, i+1) != len(raw) {
				return nil, false, false
			}
			return match, found, true
		}
		if raw[i] != ',' {
			return nil, false, false
		}
		i = skipJSONWS(raw, i+1)
	}
}

// findTopLevelUsageOrResponse scans top-level keys of a JSON response once and
// returns raw slices for "usage" and/or "response". ok == false signals an
// edge case (escaped, case-folded, duplicate key, or syntax error) where the
// caller should fall back to encoding/json.Unmarshal.
func findTopLevelUsageOrResponse(raw []byte) (usageVal, respVal []byte, ok bool) {
	i := skipJSONWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, nil, false
	}
	i = skipJSONWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return nil, nil, skipJSONWS(raw, i+1) == len(raw)
	}
	hasUsage, hasResp := false, false
	for {
		kStart := i
		kEnd, kEsc, _, scanned := scanJSONString(raw, i)
		if !scanned || kEsc {
			return nil, nil, false
		}
		key := raw[kStart+1 : kEnd-1]
		i = skipJSONWS(raw, kEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, nil, false
		}
		valStart := skipJSONWS(raw, i+1)
		valEnd, scanned := scanJSONValue(raw, valStart, 1)
		if !scanned {
			return nil, nil, false
		}
		switch len(key) {
		case 5:
			if string(key) == "usage" {
				if hasUsage {
					return nil, nil, false
				}
				usageVal = raw[valStart:valEnd]
				hasUsage = true
			} else if equalFoldASCII(key, "usage") {
				return nil, nil, false
			}
		case 8:
			if string(key) == "response" {
				if hasResp {
					return nil, nil, false
				}
				respVal = raw[valStart:valEnd]
				hasResp = true
			} else if equalFoldASCII(key, "response") {
				return nil, nil, false
			}
		}
		i = skipJSONWS(raw, valEnd)
		if i >= len(raw) {
			return nil, nil, false
		}
		if raw[i] == '}' {
			if skipJSONWS(raw, i+1) != len(raw) {
				return nil, nil, false
			}
			return usageVal, respVal, true
		}
		if raw[i] != ',' {
			return nil, nil, false
		}
		i = skipJSONWS(raw, i+1)
	}
}
