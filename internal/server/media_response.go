package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
)

type mediaResponse struct {
	Input, Output, Cache                           uint64
	TextInput, ImageInput, TextOutput, ImageOutput uint64
	TokensKnown, BreakdownKnown, ImagesKnown       bool
	Images                                         int
	Cost                                           string
	// EmbeddingDimensions is the first vector's length, when reported as an
	// array. Base64 vectors stay unknown.
	EmbeddingDimensions int
}

type MediaTokenBreakdown struct {
	TextInput   uint64 `json:"text_input"`
	ImageInput  uint64 `json:"image_input"`
	TextOutput  uint64 `json:"text_output"`
	ImageOutput uint64 `json:"image_output"`
}

// The response scanner discards media strings and vectors as it validates
// them. Unlike decoding RawMessages/Token, memory does not grow with base64.
type mediaJSON struct{ r *bufio.Reader }

func (p *mediaJSON) peek() (byte, error) {
	for {
		b, e := p.r.Peek(1)
		if e != nil {
			return 0, e
		}
		if b[0] != ' ' && b[0] != '\n' && b[0] != '\r' && b[0] != '\t' {
			return b[0], nil
		}
		p.r.ReadByte()
	}
}
func (p *mediaJSON) take(want byte) error {
	b, e := p.peek()
	if e != nil {
		return e
	}
	if b != want {
		return errors.New("invalid media JSON")
	}
	_, e = p.r.ReadByte()
	return e
}
func (p *mediaJSON) str(capture bool) (string, error) {
	if e := p.take('"'); e != nil {
		return "", e
	}
	var out bytes.Buffer
	for {
		if capture && out.Len() > 256 {
			return "", errors.New("media metadata key too long")
		}
		b, e := p.r.ReadByte()
		if e != nil {
			return "", e
		}
		if b == '"' {
			break
		}
		if b < 32 {
			return "", errors.New("invalid JSON string")
		}
		if b == '\\' {
			if capture {
				out.WriteByte(b)
			}
			b, e = p.r.ReadByte()
			if e != nil {
				return "", e
			}
			switch b {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if capture {
					out.WriteByte(b)
				}
				for range 4 {
					h, e := p.r.ReadByte()
					if e != nil {
						return "", e
					}
					if !(h >= '0' && h <= '9' || h >= 'A' && h <= 'F' || h >= 'a' && h <= 'f') {
						return "", errors.New("invalid JSON escape")
					}
					if capture {
						out.WriteByte(h)
					}
				}
				continue
			default:
				return "", errors.New("invalid JSON escape")
			}
		}
		if capture {
			if out.Len() >= 256 {
				return "", errors.New("media metadata key too long")
			}
			out.WriteByte(b)
		}
	}
	if !capture {
		return "", nil
	}
	var key string
	err := json.Unmarshal(append(append([]byte{'"'}, out.Bytes()...), '"'), &key)
	return key, err
}
func (p *mediaJSON) value(depth int) error {
	if depth > 64 {
		return errors.New("media JSON nesting too deep")
	}
	b, e := p.peek()
	if e != nil {
		return e
	}
	switch b {
	case '"':
		_, e = p.str(false)
		return e
	case '{':
		return p.object(depth+1, func(_ string) error { return p.value(depth + 1) })
	case '[':
		p.r.ReadByte()
		if b, e = p.peek(); e != nil {
			return e
		}
		if b == ']' {
			p.r.ReadByte()
			return nil
		}
		for {
			if e = p.value(depth + 1); e != nil {
				return e
			}
			b, e = p.peek()
			if e != nil {
				return e
			}
			if b == ']' {
				p.r.ReadByte()
				return nil
			}
			if e = p.take(','); e != nil {
				return e
			}
		}
	default:
		var v bytes.Buffer
		for {
			next, err := p.r.Peek(1)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if bytes.ContainsAny(next, " ,]}\t\r\n") {
				break
			}
			if v.Len() >= 128 {
				return errors.New("JSON scalar too long")
			}
			v.WriteByte(next[0])
			p.r.ReadByte()
		}
		if !json.Valid(v.Bytes()) {
			return errors.New("invalid JSON scalar")
		}
		return nil
	}
}
func (p *mediaJSON) object(depth int, field func(string) error) error {
	if e := p.take('{'); e != nil {
		return e
	}
	b, e := p.peek()
	if e != nil {
		return e
	}
	if b == '}' {
		p.r.ReadByte()
		return nil
	}
	for {
		key, e := p.str(true)
		if e != nil {
			return e
		}
		if e = p.take(':'); e != nil {
			return e
		}
		if e = field(key); e != nil {
			return e
		}
		b, e = p.peek()
		if e != nil {
			return e
		}
		if b == '}' {
			p.r.ReadByte()
			return nil
		}
		if e = p.take(','); e != nil {
			return e
		}
	}
}

func readMediaResponse(r io.Reader, operation string) (mediaResponse, error) {
	var result mediaResponse
	p := mediaJSON{bufio.NewReaderSize(r, 32<<10)}
	var usage json.RawMessage
	seen := map[string]bool{}
	items := 0
	err := p.object(0, func(key string) error {
		if key != "data" && key != "usage" {
			if key == "error" {
				return errors.New("upstream returned an error envelope")
			}
			return p.value(0)
		}
		if seen[key] {
			return errors.New("duplicate media response field")
		}
		seen[key] = true
		if key == "usage" {
			// Usage is small and bounded independently from media payloads.
			dec := json.NewDecoder(io.LimitReader(p.r, (64<<10)+1))
			if err := dec.Decode(&usage); err != nil {
				return err
			}
			if len(usage) > 64<<10 {
				return errors.New("media usage too large")
			}
			p.r = bufio.NewReader(io.MultiReader(dec.Buffered(), p.r))
			return nil
		}
		if e := p.take('['); e != nil {
			return e
		}
		c, e := p.peek()
		if e != nil {
			return e
		}
		if c == ']' {
			p.r.ReadByte()
			return nil
		}
		for {
			valid := false
			seenOutput := map[string]bool{}
			if e := p.object(0, func(k string) error {
				if operation == "images" && (k == "b64_json" || k == "url") {
					if seenOutput[k] {
						return errors.New("duplicate image output")
					}
					seenOutput[k] = true
					b, e := p.peek()
					if e != nil {
						return e
					}
					if b == 'n' {
						return p.value(0)
					}
					if b != '"' {
						return errors.New("invalid image output")
					}
					next, err := p.r.Peek(2)
					if err != nil {
						return err
					}
					if next[1] == '"' {
						return errors.New("empty image output")
					}
					valid = true
				}
				if operation == "embeddings" && k == "embedding" {
					if seenOutput[k] {
						return errors.New("duplicate embedding output")
					}
					seenOutput[k] = true
					b, e := p.peek()
					if e != nil {
						return e
					}
					if b != '[' && b != '"' {
						return errors.New("invalid embedding output")
					}
					valid = true
					if b == '[' {
						n, err := p.embedding()
						if err != nil {
							return err
						}
						if result.EmbeddingDimensions == 0 {
							result.EmbeddingDimensions = n
						}
						return nil
					}
					next, err := p.r.Peek(2)
					if err != nil {
						return err
					}
					if next[1] == '"' {
						return errors.New("empty embedding output")
					}
				}
				return p.value(0)
			}); e != nil {
				return e
			}
			if !valid {
				return errors.New("missing media output")
			}
			items++
			c, e = p.peek()
			if e != nil {
				return e
			}
			if c == ']' {
				p.r.ReadByte()
				return nil
			}
			if e = p.take(','); e != nil {
				return e
			}
		}
	})
	if err != nil {
		return result, err
	}
	if _, e := p.peek(); e != io.EOF {
		return result, errors.New("trailing media response content")
	}
	if !seen["data"] || items == 0 {
		return result, errors.New("empty media response")
	}
	if operation == "images" {
		result.Images = items
		result.ImagesKnown = true
	}
	if len(usage) > 0 && string(usage) != "null" {
		if err := decodeMediaUsage(usage, operation, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (p *mediaJSON) embedding() (int, error) {
	if err := p.take('['); err != nil {
		return 0, err
	}
	dimensions := 0
	for {
		b, err := p.peek()
		if err != nil {
			return 0, err
		}
		if b != '-' && (b < '0' || b > '9') {
			return 0, errors.New("invalid embedding vector")
		}
		var num bytes.Buffer
		for {
			next, err := p.r.Peek(1)
			if err != nil {
				return 0, err
			}
			if bytes.ContainsAny(next, ",] \t\r\n") {
				break
			}
			if num.Len() >= 128 {
				return 0, errors.New("embedding scalar too long")
			}
			num.WriteByte(next[0])
			p.r.ReadByte()
		}
		f, err := strconv.ParseFloat(num.String(), 64)
		if err != nil || math.IsInf(f, 0) || !json.Valid(num.Bytes()) {
			return 0, errors.New("invalid embedding number")
		}
		dimensions++
		b, err = p.peek()
		if err != nil {
			return 0, err
		}
		if b == ']' {
			p.r.ReadByte()
			return dimensions, nil
		}
		if err = p.take(','); err != nil {
			return 0, err
		}
	}
}

func decodeMediaUsage(raw []byte, operation string, u *mediaResponse) error {
	var p struct {
		PromptTokens      *uint64      `json:"prompt_tokens"`
		CompletionTokens  *uint64      `json:"completion_tokens"`
		InputTokens       *uint64      `json:"input_tokens"`
		OutputTokens      *uint64      `json:"output_tokens"`
		PromptDetails     tokenDetails `json:"prompt_tokens_details"`
		InputDetails      tokenDetails `json:"input_tokens_details"`
		CompletionDetails tokenDetails `json:"completion_tokens_details"`
		OutputDetails     tokenDetails `json:"output_tokens_details"`
		Cost              json.Number  `json:"cost"`
	}
	if e := json.Unmarshal(raw, &p); e != nil {
		return errors.New("invalid upstream usage")
	}
	in, out, details, outDetails := p.InputTokens, p.OutputTokens, p.InputDetails, p.OutputDetails
	if in == nil {
		in, out, details, outDetails = p.PromptTokens, p.CompletionTokens, p.PromptDetails, p.CompletionDetails
	}
	if in != nil && (out != nil || operation == "embeddings") {
		u.Input = *in
		if out != nil && operation != "embeddings" {
			u.Output = *out
		}
		u.Cache = details.Cached
		if u.Cache > u.Input {
			return errors.New("invalid cached token count")
		}
		if u.Input > 1<<53-1 || u.Output > 1<<53-1 {
			return errors.New("upstream token count too large")
		}
		u.TokensKnown = true
		if details.Text != nil && details.Image != nil && outDetails.Image != nil {
			u.TextInput, u.ImageInput, u.ImageOutput = *details.Text, *details.Image, *outDetails.Image
			if outDetails.Text != nil {
				u.TextOutput = *outDetails.Text
			}
			u.BreakdownKnown = u.TextInput <= u.Input && u.ImageInput == u.Input-u.TextInput && u.TextOutput <= u.Output && u.ImageOutput == u.Output-u.TextOutput
		}
	}
	if p.Cost != "" {
		if _, ok := decimalRate(p.Cost.String()); !ok {
			return errors.New("invalid upstream cost")
		}
		u.Cost = p.Cost.String()
	}
	return nil
}

type tokenDetails struct {
	Cached uint64  `json:"cached_tokens"`
	Text   *uint64 `json:"text_tokens"`
	Image  *uint64 `json:"image_tokens"`
}
