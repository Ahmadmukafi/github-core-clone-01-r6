// Raw HTTP/1.1 server: request parsing, keep-alive, chunked bodies, and the
// route table. Written for the forge's hot path — no standard-library HTTP
// server overhead, explicit Content-Length on every response.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const maxBodyBytes = 64 * 1024 * 1024

type httpRequest struct {
	Method  string
	Path    string // raw path, no query
	Query   QueryParams
	Headers map[string]string
	Body    []byte
	IsHead  bool
}

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type forgeServer struct {
	store    *Store
	listener net.Listener
}

func newForgeServer(store *Store) *forgeServer {
	return &forgeServer{store: store}
}

func (s *forgeServer) listen(addr string) (int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	s.listener = ln
	port := ln.Addr().(*net.TCPAddr).Port
	go s.acceptLoop()
	return port, nil
}

func (s *forgeServer) close() {
	if s.listener != nil {
		s.listener.Close()
	}
}

func (s *forgeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		// Disable Nagle: the forge answers many small request/response pairs
		// on one keep-alive connection, and a delayed second segment costs
		// a round trip. Node's server does the same by default.
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go s.handleConn(conn)
	}
}

func (s *forgeServer) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 32768)
	for {
		req, keepAlive, err := s.readRequest(reader, conn)
		if err != nil {
			return
		}
		result := s.dispatch(req)
		if !writeResponse(conn, req, result) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRequest parses one request from the connection. It returns the request
// and whether the connection stays open afterwards.
func (s *forgeServer) readRequest(reader *bufio.Reader, conn net.Conn) (*httpRequest, bool, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, false, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, fmt.Errorf("bad request line")
	}
	method, rawURI, version := parts[0], parts[1], parts[2]
	if !strings.HasPrefix(version, "HTTP/") {
		return nil, false, fmt.Errorf("bad http version")
	}
	// Absolute-form URIs (http://host/path) collapse to the path.
	if strings.HasPrefix(rawURI, "http://") || strings.HasPrefix(rawURI, "https://") {
		rest := rawURI[strings.Index(rawURI, "://")+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			rawURI = rest[slash:]
		} else {
			rawURI = "/"
		}
	}
	path := rawURI
	queryRaw := ""
	if q := strings.IndexByte(rawURI, '?'); q >= 0 {
		path = rawURI[:q]
		queryRaw = rawURI[q+1:]
	}
	headers := map[string]string{}
	for {
		hline, err := reader.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if idx := strings.IndexByte(hline, ':'); idx > 0 {
			name := strings.ToLower(strings.TrimSpace(hline[:idx]))
			value := strings.TrimSpace(hline[idx+1:])
			if _, exists := headers[name]; !exists {
				headers[name] = value
			}
		}
	}
	// Expect: 100-continue — answer before reading the body.
	if strings.EqualFold(headers["expect"], "100-continue") {
		conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	var body []byte
	if te := headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
		body, err = readChunkedBody(reader)
		if err != nil {
			return nil, false, err
		}
	} else if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, false, fmt.Errorf("bad content-length")
		}
		if n > maxBodyBytes {
			return nil, false, fmt.Errorf("request body too large")
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(reader, body); err != nil {
				return nil, false, err
			}
		}
	}
	keepAlive := true
	if strings.EqualFold(headers["connection"], "close") {
		keepAlive = false
	} else if version == "HTTP/1.0" && !strings.EqualFold(headers["connection"], "keep-alive") {
		keepAlive = false
	}
	req := &httpRequest{
		Method:  method,
		Path:    path,
		Query:   parseQueryParams(queryRaw),
		Headers: headers,
		Body:    body,
		IsHead:  method == "HEAD",
	}
	return req, keepAlive, nil
}

func readChunkedBody(reader *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if idx := strings.IndexByte(line, ';'); idx >= 0 {
			line = line[:idx]
		}
		size, err := strconv.ParseInt(line, 16, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad chunk size")
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				tline, err := reader.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(tline, "\r\n") == "" {
					break
				}
			}
			return body, nil
		}
		if len(body)+int(size) > maxBodyBytes {
			return nil, fmt.Errorf("request body too large")
		}
		chunk := make([]byte, size+2)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk[:size]...)
	}
}

// parseQueryParams mirrors URLSearchParams: '+' becomes a space, percent
// escapes decode leniently, and the first value wins for Get.
func parseQueryParams(raw string) QueryParams {
	out := QueryParams{}
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			key, value = pair[:idx], pair[idx+1:]
		}
		key = queryUnescape(key)
		value = queryUnescape(value)
		out[key] = append(out[key], value)
	}
	return out
}

func queryUnescape(s string) string {
	// URLSearchParams decodes '+' as a space and leaves invalid % sequences
	// literal.
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
			continue
		}
		if c == '%' && i+2 < len(s) {
			hi, ok1 := hexDigit(s[i+1])
			lo, ok2 := hexDigit(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// decodeURIComponent decodes a path segment; malformed escapes are an error.
func decodeURIComponent(s string) (string, error) {
	return url.PathUnescape(s)
}

func jsonResult(status int, body string) HttpResult {
	return HttpResult{Status: status, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(body)}
}

func textPlainResult(status int, body string) HttpResult {
	return HttpResult{Status: status, Headers: map[string]string{"Content-Type": "text/plain"}, Body: []byte(body)}
}

func (s *forgeServer) dispatch(req *httpRequest) (result HttpResult) {
	defer func() {
		if r := recover(); r != nil {
			result = serverErrorResult()
		}
	}()
	store := s.store

	if req.Path == "/healthz" {
		if req.Method != "GET" {
			return jsonResult(404, `{"message":"Not Found"}`)
		}
		return jsonResult(200, `{"ok":true}`)
	}

	if req.Path == "/api/graphql" {
		if req.Method != "POST" {
			return jsonResult(404, `{"message":"Not Found"}`)
		}
		store.Mu.Lock()
		defer store.Mu.Unlock()
		return handleGraphql(store, req.Headers, req.Body)
	}

	if req.Path == "/mcp" {
		if req.Method != "POST" {
			return jsonResult(404, `{"message":"Not Found"}`)
		}
		store.Mu.Lock()
		defer store.Mu.Unlock()
		return handleMcp(store, req.Headers, req.Body)
	}

	if strings.HasPrefix(req.Path, "/api/v3") {
		rawSegments := strings.Split(req.Path, "/")
		var segments []string
		if len(rawSegments) > 3 {
			for _, seg := range rawSegments[3:] {
				if seg == "" {
					continue
				}
				decoded, err := decodeURIComponent(seg)
				if err != nil {
					segments = append(segments, seg)
				} else {
					segments = append(segments, decoded)
				}
			}
		}
		restReq := RestRequest{
			Method:   req.Method,
			Segments: segments,
			Query:    req.Query,
			Headers:  req.Headers,
			Body:     req.Body,
		}
		if req.Method == "GET" || req.Method == "HEAD" {
			restReq.Body = nil
		}
		store.Mu.Lock()
		defer store.Mu.Unlock()
		return handleRest(store, restReq)
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if owner, repoName, action, ok := matchGitPath(req.Path); ok {
		ownerDec, err1 := decodeURIComponent(owner)
		repoDec, err2 := decodeURIComponent(repoName)
		if err1 != nil || err2 != nil {
			return serverErrorResult()
		}
		repo := getRepo(store, ownerDec, repoDec)
		if repo == nil {
			return textPlainResult(404, "repository "+ownerDec+"/"+repoDec+" not found")
		}
		if action == "info/refs" && req.Method == "GET" {
			service := req.Query.Get("service")
			if service == "" {
				return textPlainResult(400, "service parameter is required")
			}
			store.Mu.RLock()
			defer store.Mu.RUnlock()
			return infoRefs(repo, service, req.Headers["git-protocol"])
		}
		if action == "git-upload-pack" && req.Method == "POST" {
			store.Mu.RLock()
			defer store.Mu.RUnlock()
			return handleUploadPack(repo, req.Body)
		}
		if action == "git-receive-pack" && req.Method == "POST" {
			store.Mu.Lock()
			defer store.Mu.Unlock()
			return handleReceivePack(repo, req.Body)
		}
		return textPlainResult(404, "not found")
	}

	return textPlainResult(404, "not found")
}

// matchGitPath implements /^\/([^/]+)\/([^/]+)\.git\/(.+)$/.
func matchGitPath(path string) (owner, repoName, action string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) < 3 {
		return "", "", "", false
	}
	owner = parts[0]
	repoPart := parts[1]
	if owner == "" || repoPart == "" {
		return "", "", "", false
	}
	if !strings.HasSuffix(repoPart, ".git") {
		return "", "", "", false
	}
	repoName = repoPart[:len(repoPart)-len(".git")]
	if repoName == "" {
		return "", "", "", false
	}
	action = strings.Join(parts[2:], "/")
	if action == "" {
		return "", "", "", false
	}
	return owner, repoName, action, true
}

func writeResponse(conn net.Conn, req *httpRequest, result HttpResult) bool {
	reason, ok := reasonPhrases[result.Status]
	if !ok {
		reason = "OK"
	}
	// One buffer, one write: headers and body leave together, so Nagle
	// never holds the body behind the header segment.
	buf := make([]byte, 0, 256+len(result.Body))
	buf = append(buf, "HTTP/1.1 "...)
	buf = append(buf, strconv.Itoa(result.Status)...)
	buf = append(buf, ' ')
	buf = append(buf, reason...)
	buf = append(buf, "\r\n"...)
	for k, v := range result.Headers {
		buf = append(buf, k...)
		buf = append(buf, ": "...)
		buf = append(buf, v...)
		buf = append(buf, "\r\n"...)
	}
	buf = append(buf, "Content-Length: "...)
	buf = append(buf, strconv.Itoa(len(result.Body))...)
	buf = append(buf, "\r\n\r\n"...)
	if !req.IsHead {
		buf = append(buf, result.Body...)
	}
	_, err := conn.Write(buf)
	return err == nil
}
