package driver

import (
	"bytes"
	"errors"
	"strconv"
)

// Response is a parsed answer. A requester keeps one and refills it, so
// checking an answer costs nothing that would show up in the latency it is
// timing.
type Response struct {
	Status int
	Body   []byte // Points into the read buffer, valid until the next read.
}

var (
	errShortResponse   = errors.New("response incomplete")
	errMalformedStatus = errors.New("malformed status line")
	errMissingLength   = errors.New("response without Content-Length")
)

// ParseResponse parses one answer out of buf and returns how many bytes it
// took, so a requester reading a keep-alive connection knows where the next one
// starts. It allocates nothing.
//
// A Content-Length is required: an answer delimited by the connection closing
// costs a round trip that would otherwise be charged to the server, and every
// implementation here frames its answers.
func ParseResponse(dst *Response, buf []byte) (int, error) {
	*dst = Response{}
	headerEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0, errShortResponse
	}
	head := buf[:headerEnd]
	line, rest := nextLine(head)
	// "HTTP/1.1 200 OK": the code is the second field.
	sp := bytes.IndexByte(line, ' ')
	if sp < 0 || len(line) < sp+4 {
		return 0, errMalformedStatus
	}
	code, err := strconv.Atoi(string(line[sp+1 : sp+4]))
	if err != nil {
		return 0, errMalformedStatus
	}
	dst.Status = code

	contentLength := -1
	for len(rest) > 0 {
		line, rest = nextLine(rest)
		key, value := splitField(line)
		if equalFold(key, "content-length") {
			contentLength, err = strconv.Atoi(string(value))
			if err != nil {
				return 0, errMissingLength
			}
		}
	}
	if contentLength < 0 {
		return 0, errMissingLength
	}
	bodyStart := headerEnd + len("\r\n\r\n")
	total := bodyStart + contentLength
	if len(buf) < total {
		return 0, errShortResponse
	}
	dst.Body = buf[bodyStart:total]
	return total, nil
}

// Check reports whether an answer is the one the request asked for.
//
// TODO(corpus): this only checks the status and the bytes echoed back. Once the
// corpus moves into this module, a server will answer with a digest of
// everything it parsed and this will check that instead, which is what makes
// the benchmark a conformance test as well.
func (s *RequestSet) Check(variant int, resp *Response) error {
	if s.IsProbe() {
		return nil // Whatever it answered is the measurement.
	}
	if resp.Status != s.wantStatus {
		return errors.New("answered " + strconv.Itoa(resp.Status) +
			", want " + strconv.Itoa(s.wantStatus))
	}
	want := s.wantBody[variant]
	if want == nil {
		return nil
	}
	if !bytes.Equal(resp.Body, want) {
		return errors.New("answered " + strconv.Itoa(len(resp.Body)) +
			" body bytes, want " + strconv.Itoa(len(want)) + " identical ones")
	}
	return nil
}

func nextLine(b []byte) (line, rest []byte) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return b, nil
	}
	line = b[:i]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line, b[i+1:]
}

func splitField(line []byte) (key, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	value = line[i+1:]
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	return line[:i], value
}

func equalFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if lowerASCII(b[i]) != lowerASCII(s[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
