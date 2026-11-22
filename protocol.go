package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"
)

type wireLimits struct{ header, body, depth, queued int }

var draftWire = wireLimits{1024, 16 << 20, 64, 16 << 20}
var errFrame = errors.New("invalid RPC frame")
var errJSON = errors.New("invalid RPC JSON")
var errQuota = errors.New("RPC message exceeds negotiated limits")
var errQueue = errors.New("RPC output queue is full")

func readFrame(r *bufio.Reader, limits wireLimits) ([]byte, error) {
	const prefix = "Content-Length: "
	const suffix = "\r\n\r\n"
	header := make([]byte, 0, 64)
	for !bytes.HasSuffix(header, []byte(suffix)) {
		if len(header) == limits.header {
			return nil, errFrame
		}
		c, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		header = append(header, c)
	}

	field, ok := strings.CutPrefix(string(header[:len(header)-len(suffix)]), prefix)
	maxBody := min(limits.body, limits.queued-len(header))
	length := 0
	for _, c := range field {
		if c < '0' || c > '9' || length > maxBody/10 {
			return nil, errFrame
		}
		length = length*10 + int(c-'0')
		if length > maxBody {
			return nil, errFrame
		}
	}
	if !ok || length == 0 {
		return nil, errFrame
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	if !validJSON(body, limits.depth) {
		return nil, errJSON
	}
	return body, nil
}

func validJSON(body []byte, maxDepth int) bool {
	if !utf8.Valid(body) || !pairedSurrogates(body) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tokens := 0
	var value func(json.Token, int) bool
	value = func(token json.Token, depth int) bool {
		tokens++
		if tokens > 32768 {
			return false
		}
		switch t := token.(type) {
		case json.Delim:
			if depth > maxDepth || t != '{' && t != '[' {
				return false
			}
			var keys map[string]struct{}
			if t == '{' {
				keys = make(map[string]struct{})
			}
			for dec.More() {
				if keys != nil {
					key, err := dec.Token()
					name, ok := key.(string)
					tokens++
					if err != nil || !ok || tokens > 32768 {
						return false
					}
					if _, exists := keys[name]; exists {
						return false
					}
					keys[name] = struct{}{}
				}
				next, err := dec.Token()
				if err != nil || !value(next, depth+1) {
					return false
				}
			}
			_, err := dec.Token()
			return err == nil
		case json.Number:
			_, err := t.Float64()
			return err == nil
		default:
			return true
		}
	}
	token, err := dec.Token()
	if err != nil || !value(token, 1) {
		return false
	}
	_, err = dec.Token()
	return err == io.EOF
}

// encoding/json accepts unpaired Unicode surrogate escapes and replaces them.
func pairedSurrogates(body []byte) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 == len(body) {
			continue
		}
		i++
		if body[i] != 'u' || i+4 >= len(body) {
			continue
		}
		code, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		switch {
		case code >= 0xd800 && code <= 0xdbff:
			if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		case code >= 0xdc00 && code <= 0xdfff:
			return false
		}
	}
	return true
}

type fields map[string]json.RawMessage

func decode[T any](f fields, key string, dst *T) bool {
	raw, ok := f[key]
	return ok && !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, dst) == nil
}

func rpcID(id, prefix string) bool {
	digits, ok := strings.CutPrefix(id, prefix)
	if !ok || digits == "" || digits[0] == '0' {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type frame struct {
	header, body []byte
	cancel       <-chan struct{}
	final        bool
}

type output struct {
	frames  chan frame
	done    chan error
	pending atomic.Int64
}

func newOutput(writer io.Writer) *output {
	out := &output{frames: make(chan frame, 4), done: make(chan error, 1)}
	go func() {
		for f := range out.frames {
			select {
			case <-f.cancel:
			default:
				if err := writeFrame(writer, f.header, f.body); err != nil {
					out.done <- err
					return
				}
			}
			out.pending.Add(-int64(len(f.header) + len(f.body)))
			if f.final {
				out.done <- nil
				return
			}
		}
	}()
	return out
}

func writeFrame(writer io.Writer, parts ...[]byte) error {
	for _, part := range parts {
		for len(part) != 0 {
			n, err := writer.Write(part)
			part = part[n:]
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
	}
	return nil
}

func (o *output) send(limits wireLimits, msg any, cancel <-chan struct{}, final bool) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(body))
	size := int64(len(body) + len(header))
	if len(body) > limits.body || len(header) > limits.header || size > int64(limits.queued) ||
		!validJSON(body, limits.depth) {
		return errQuota
	}
	if o.pending.Add(size) > int64(limits.queued) {
		o.pending.Add(-size)
		return errQueue
	}
	select {
	case o.frames <- frame{header, body, cancel, final}:
		return nil
	default:
		o.pending.Add(-size)
		return errQueue
	}
}
