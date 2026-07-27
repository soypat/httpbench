// Package driver sends the load a server under test is measured with, and
// checks what comes back.
//
// It lives here, out of the root package, so that no server binary can link it:
// the size of those binaries is one of the results, and it only means anything
// if a server contains the server and nothing else.
//
// The requests are generated here rather than taken from a shared corpus. That
// is a placeholder: the corpus, its digest and the conformance run that
// verifies a server answered correctly before any row is reported still have to
// move into this module, and this package's checking is thin until they do.
package driver

import (
	"strconv"

	"github.com/soypat/httpbench"
)

// Routes every implementation serves.
const (
	RouteHello     = "/hello-world"
	RouteEcho      = "/echo"
	RouteQuery     = "/query"
	RouteForm      = "/form"
	RouteMultipart = "/multipart"
)

// Scenario names, which are also what a report calls its rows.
const (
	ScenarioHello     = "hello"
	ScenarioEcho      = "echo"
	ScenarioFlood     = "headerflood"
	ScenarioQuery     = "query"
	ScenarioForm      = "form"
	ScenarioMultipart = "multipart"
)

// RequestSet is a scenario's requests, serialized once. A requester that
// formats requests while it measures is measuring itself, so every byte a
// connection sends is prepared before the clock starts.
type RequestSet struct {
	Scenario string
	// wire is one whole request per variant, ready to be written. Several
	// variants are used so a server cannot look good by accident on one shape
	// of request.
	wire [][]byte
	// wantBody is what the answer's body must be, per variant. A nil entry
	// means the body is not checked, only its framing.
	wantBody [][]byte
	// wantStatus is the status an answer must carry, or zero for a probe.
	wantStatus int
}

// IsProbe reports whether the set measures the parse rather than the answer.
// A server refusing a probe is a result, not a failure: whether a request that
// large is served at all is a matter of how the server was configured.
func (s *RequestSet) IsProbe() bool { return s.wantStatus == 0 }

// WireBytes returns the average request size, for the amplification columns.
func (s *RequestSet) WireBytes() int {
	total := 0
	for _, w := range s.wire {
		total += len(w)
	}
	return total / len(s.wire)
}

// Variants returns how many shapes of request the set holds.
func (s *RequestSet) Variants() int { return len(s.wire) }

// HelloSet asks for the smallest answer a server can give. What it measures is
// the fixed cost of an exchange: everything a server spends before a handler
// has done anything worth doing.
func HelloSet(variants int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioHello, wantStatus: 200}
	body := []byte("hello world")
	for i := 0; i < variants; i++ {
		req := append([]byte("GET "+RouteHello+" HTTP/1.1\r\nHost: bench\r\n"), noiseHeaders(nil, i)...)
		set.wire = append(set.wire, append(req, '\r', '\n'))
		set.wantBody = append(set.wantBody, body)
	}
	return set
}

// EchoSet sends a body of bodyBytes and expects it back. It is the scenario
// where a server's per-request memory is a function of what it was sent, which
// is the shape of every real workload.
func EchoSet(variants, bodyBytes int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioEcho + "/" + strconv.Itoa(bodyBytes), wantStatus: 200}
	for i := 0; i < variants; i++ {
		body := make([]byte, bodyBytes)
		fill(body, uint64(i+1))
		req := []byte("POST " + RouteEcho + " HTTP/1.1\r\nHost: bench\r\nContent-Type: application/octet-stream\r\nContent-Length: ")
		req = strconv.AppendInt(req, int64(bodyBytes), 10)
		req = append(req, '\r', '\n')
		req = append(req, noiseHeaders(nil, i)...)
		req = append(req, '\r', '\n')
		set.wire = append(set.wire, append(req, body...))
		set.wantBody = append(set.wantBody, body)
	}
	return set
}

// FloodSet asks for the smallest answer behind the largest header block, which
// is where a server that parses into memory it took up front and one that
// allocates per field stop resembling each other.
//
// It is a probe: a bounded server answers 431 and an unbounded one answers 200,
// and the cost of reaching either is the measurement.
func FloodSet(variants, blockBytes int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioFlood}
	for i := 0; i < variants; i++ {
		req := []byte("GET " + RouteHello + " HTTP/1.1\r\nHost: bench\r\n")
		for n := 0; len(req) < blockBytes; n++ {
			req = append(req, "X-Flood-"...)
			req = strconv.AppendInt(req, int64(n), 36)
			req = append(req, ": "...)
			req = append(req, floodValue...)
			req = append(req, '\r', '\n')
		}
		set.wire = append(set.wire, append(req, '\r', '\n'))
		set.wantBody = append(set.wantBody, nil)
	}
	return set
}

// QuerySet asks for one parameter out of a query string that holds several,
// percent-encoded, and expects it back decoded. It is the cheapest scenario
// that makes a server parse rather than copy, and the last variant leaves the
// parameter out so the absent case is measured too — a server that only ever
// sees the parameter present is not being asked the harder half of the
// question.
func QuerySet(variants int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioQuery, wantStatus: 200}
	for i := 0; i < variants; i++ {
		target := []byte(RouteQuery + "?first=" + strconv.Itoa(i) + "&")
		want := notPresentBody
		if i < variants-1 { // Last variant omits the parameter entirely.
			value := queryValues[i%len(queryValues)]
			target = formEncode(append(target, "query="...), value)
			target = append(target, '&')
			want = []byte(value)
		}
		target = append(target, "last=end"...)
		req := append([]byte("GET "), target...)
		req = append(req, " HTTP/1.1\r\nHost: bench\r\n"...)
		req = append(req, noiseHeaders(nil, i)...)
		set.wire = append(set.wire, append(req, '\r', '\n'))
		set.wantBody = append(set.wantBody, want)
	}
	return set
}

// FormSet posts an urlencoded body and expects every pair back, decoded, one
// per line. Keys and values both carry escapes, since a form parser that
// decodes only values is a parser that has not been tested.
//
// The pairs are sent in the order their decoded keys sort in. A server that
// keeps wire order and one that keeps its pairs in a map then sorts them agree
// on this body and on no other, which is what lets one expected answer be
// checked against both.
func FormSet(variants int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioForm, wantStatus: 200}
	for i := 0; i < variants; i++ {
		var body, want []byte
		for j, key := range formKeys {
			value := formValues[(i+j)%len(formValues)]
			if j > 0 {
				body = append(body, '&')
			}
			body = formEncode(append(formEncode(body, key), '='), value)
			want = append(append(append(append(want, key...), '='), value...), '\n')
		}
		req := []byte("POST " + RouteForm + " HTTP/1.1\r\nHost: bench\r\n" +
			"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: ")
		req = strconv.AppendInt(req, int64(len(body)), 10)
		req = append(req, "\r\n\r\n"...)
		set.wire = append(set.wire, append(req, body...))
		set.wantBody = append(set.wantBody, want)
	}
	return set
}

// MultipartSet posts a "multipart/form-data" body and expects one line per
// part: its name, its filename after a ';' when it has one, and its content.
//
// It is the scenario a streaming parser is built for. Parts declare no length,
// so a server either reads them through a buffer it owns or holds the whole
// body — and one part's content carries a CRLF and a line that starts like a
// delimiter without being one, so a parser that scans for "--" rather than for
// the boundary answers wrongly rather than slowly.
func MultipartSet(variants int) *RequestSet {
	set := &RequestSet{Scenario: ScenarioMultipart, wantStatus: 200}
	for i := 0; i < variants; i++ {
		boundary := "httpbench" + strconv.Itoa(i) + "Boundary"
		var body, want []byte
		for j, part := range multipartParts {
			content := part.content + strconv.Itoa(i)
			body = append(body, "--"+boundary+"\r\nContent-Disposition: form-data; name=\""+part.name+"\""...)
			want = append(want, part.name...)
			if part.filename != "" {
				body = append(body, "; filename=\""+part.filename+"\""...)
				want = append(append(want, ';'), part.filename...)
			}
			body = append(body, "\r\nContent-Type: "+multipartContentType(j)+"\r\n\r\n"...)
			body = append(append(body, content...), '\r', '\n')
			want = append(append(append(want, '='), content...), '\n')
		}
		body = append(body, "--"+boundary+"--\r\n"...)
		req := []byte("POST " + RouteMultipart + " HTTP/1.1\r\nHost: bench\r\n" +
			"Content-Type: multipart/form-data; boundary=" + boundary + "\r\nContent-Length: ")
		req = strconv.AppendInt(req, int64(len(body)), 10)
		req = append(req, "\r\n\r\n"...)
		set.wire = append(set.wire, append(req, body...))
		set.wantBody = append(set.wantBody, want)
	}
	return set
}

// notPresentBody is what a server answers when the query parameter is absent.
// Both implementations say it in the same words so the answer can be checked
// rather than merely counted.
var notPresentBody = []byte("not present")

// queryValues are values that survive a round trip only if the server decoded
// them: a space, a '+' that is a plus rather than a space once encoded, the
// separators of the encoding itself, and bytes above ASCII.
var queryValues = []string{
	"hello world",
	"a+b=c&d",
	"100% sure",
	"ünïcøde/ø",
	"tab\tand slash/",
}

var (
	// formKeys are in the order their decoded forms sort in; the first carries
	// an escape so key decoding is exercised too.
	formKeys   = [...]string{"a b", "alpha", "beta"}
	formValues = [...]string{"one two", "=&%", "ünïcøde", "", "trailing "}
)

var multipartParts = [...]struct{ name, filename, content string }{
	{name: "alpha", content: "first part "},
	// Content that lies about where it ends: a bare "--" line and a CRLF.
	{name: "beta", content: "line one\r\n--not-the-boundary\r\nline two "},
	{name: "file", filename: "note.txt", content: "file contents "},
}

// multipartContentType varies what parts declare, since a part's own
// Content-Type is a field the parser must skip past to reach the content.
func multipartContentType(part int) string {
	if part%2 == 0 {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// formEncode appends s to dst percent-encoded the way a form is: unreserved
// bytes as themselves, a space as '+', everything else as an escape.
func formEncode(dst []byte, s string) []byte {
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			dst = append(dst, '+')
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			dst = append(dst, c)
		default:
			dst = append(dst, '%', hexDigits[c>>4], hexDigits[c&0xf])
		}
	}
	return dst
}

// MetricsRequest is the out-of-band sample request. It is written on its own
// connection, outside every measured window.
var MetricsRequest = []byte("GET " + httpbench.RouteMetrics + " HTTP/1.1\r\nHost: bench\r\nConnection: close\r\n\r\n")

const floodValue = "0123456789abcdef0123456789abcdef"

// noiseHeaders appends header fields a real client would send, so a request is
// not measured against a parser that only ever sees two fields.
func noiseHeaders(dst []byte, seed int) []byte {
	fields := [...]string{
		"User-Agent: httpbench/1\r\n",
		"Accept: */*\r\n",
		"Accept-Encoding: gzip, deflate\r\n",
		"Accept-Language: en-US,en;q=0.9\r\n",
		"Cache-Control: no-cache\r\n",
	}
	for i := 0; i <= seed%len(fields); i++ {
		dst = append(dst, fields[i]...)
	}
	return dst
}

// fill writes deterministic bytes so an echoed body can be compared without
// keeping a second copy of it around.
func fill(dst []byte, seed uint64) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	rng := seed
	for i := range dst {
		rng = rng*6364136223846793005 + 1442695040888963407
		dst[i] = alphabet[(rng>>33)%uint64(len(alphabet))]
	}
}
