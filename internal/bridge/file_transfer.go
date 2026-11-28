package bridge

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type fileInfo struct {
	ID     string                             `json:"id"`
	Name   string                             `json:"name"`
	Size   *int64                             `json:"size"`
	MIME   string                             `json:"mimeType"`
	Digest *struct{ Algorithm, Value string } `json:"digest"`
}
type uploadForm struct {
	Fields    []struct{ Name, Value string } `json:"fields"`
	FileField string                         `json:"fileField"`
}
type fileTicket struct {
	Actor, Source, Tool, Origin, URL, Method string
	Auth                                     bool
	Expires                                  int64
	File                                     fileInfo
	Headers                                  map[string]string
	Form                                     *uploadForm
}

var transferPath = regexp.MustCompile(`^(/[a-zA-Z0-9._~-]+)+$`)
var headerName = regexp.MustCompile("^[!#$%&'*+.^_~|0-9a-zA-Z-]+$")

var transferClient = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	return &http.Client{Transport: tr, CheckRedirect: rejectRedirect, Timeout: 30 * time.Minute}
}()

func httpsPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	return "443"
}

func fileOriginURL(endpoint, raw, path string) (*url.URL, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || base.User != nil {
		return nil, invalid("invalid upstream MCP address")
	}
	if path != "" {
		if raw != "" || !transferPath.MatchString(path) {
			return nil, invalid("invalid upstream file path")
		}
		for _, part := range strings.Split(path, "/") {
			if part == "." || part == ".." {
				return nil, invalid("invalid upstream file path")
			}
		}
		if base.Scheme != "https" && !(base.Scheme == "http" && (net.ParseIP(base.Hostname()).IsLoopback() || base.Hostname() == "localhost")) {
			return nil, invalid("file path requires HTTPS or a loopback source")
		}
		base.Path = strings.TrimRight(base.Path, "/") + path
		base.RawPath = ""
		return base, nil
	}
	addr, err := url.Parse(raw)
	if err != nil || addr.User != nil || addr.Scheme != "https" || addr.Fragment != "" || base.Scheme != "https" || !strings.EqualFold(base.Hostname(), addr.Hostname()) || httpsPort(base) != httpsPort(addr) {
		return nil, invalid("credentialed file URL must use configured upstream origin")
	}
	return addr, nil
}

func (b *Bridge) fileTargets(ctx context.Context, p Principal, h routeHandle, result Object, pending map[string]fileInfo) error {
	meta, _ := result["_meta"].(map[string]any)
	files, _ := meta[filesExt].(map[string]any)
	for _, group := range []struct {
		value    any
		download bool
	}{{result["uploads"], false}, {files["downloads"], true}} {
		if group.value == nil {
			continue
		}
		targets, ok := group.value.([]any)
		if !ok {
			return invalid("invalid file targets")
		}
		if !group.download && (len(targets) != len(pending) || len(targets) == 0) {
			return invalid("upload targets do not match file arguments")
		}
		seen := map[string]bool{}
		for _, item := range targets {
			t, ok := item.(map[string]any)
			if !ok {
				return invalid("invalid file target")
			}
			for k, v := range t {
				if v == nil {
					delete(t, k)
					continue
				}
				switch k {
				case "url", "path", "credentials", "expiresAt", "fileId", "method", "name", "mimeType":
					if _, ok := v.(string); !ok {
						return invalid("invalid file target field")
					}
				case "form", "digest", "headers", "size", "_meta":
				default:
					return invalid("unknown file target field")
				}
				if group.download && (k == "fileId" || k == "form" || k == "method") || !group.download && (k == "name" || k == "mimeType" || k == "size" || k == "digest") {
					return invalid("unexpected file target field")
				}
			}
			id, _ := t["fileId"].(string)
			if !group.download {
				if _, ok := pending[id]; !ok || seen[id] {
					return invalid("unmatched or duplicate upload target")
				}
				seen[id] = true
			}
			address, _ := t["url"].(string)
			path, _ := t["path"].(string)
			if (path == "") == (address == "") {
				return invalid("expected exactly one file address")
			}
			if t["credentials"] != "include" && t["credentials"] != "omit" {
				return invalid("invalid target credentials")
			}
			var err error
			f := fileTicket{Actor: p.Name, Origin: h.Origin, Source: h.Source, Tool: h.Tool, Auth: t["credentials"] == "include", Expires: time.Now().Add(2 * time.Hour).Unix()}
			if h.Expires != 0 {
				f.Expires = min(f.Expires, h.Expires)
			}
			if value, ok := t["expiresAt"].(string); ok {
				d, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return invalid("invalid file expiry")
				}
				f.Expires = min(f.Expires, d.Unix())
			}
			if group.download {
				f.Method = "GET"
				raw, _ := json.Marshal(t)
				if err = json.Unmarshal(raw, &f.File); err != nil {
					return invalid("invalid download metadata")
				}
			} else {
				f.Method = "PUT"
				if m, ok := t["method"].(string); ok {
					f.Method = m
				}
				if f.Method != "POST" && f.Method != "PUT" {
					return invalid("invalid upload method")
				}
				f.File = pending[id]
			}
			if f.File.Size == nil || *f.File.Size < 0 {
				return invalid("invalid or unsupported transfer size")
			}
			if group.download && f.File.MIME == "" {
				return invalid("missing download media type")
			}
			if f.File.Digest != nil && f.File.Digest.Algorithm != "sha-256" {
				return invalid("unsupported file digest")
			}
			if raw := t["headers"]; raw != nil {
				data, _ := json.Marshal(raw)
				if json.Unmarshal(data, &f.Headers) != nil {
					return invalid("invalid target headers")
				}
				for k, v := range f.Headers {
					l := strings.ToLower(k)
					if !headerName.MatchString(k) || strings.IndexFunc(v, func(c rune) bool { return c < 32 && c != 9 || c == 127 }) >= 0 || strings.HasPrefix(l, "access-control-request-") || strings.HasPrefix(l, "x-forwarded-") || strings.HasPrefix(l, "proxy-") || strings.HasPrefix(l, "sec-") {
						return invalid("invalid transfer header")
					}
					switch l {
					case "accept-charset", "access-control-request-headers", "access-control-request-method", "date", "dnt", "permissions-policy", "cookie2", "authorization", "cookie", "host", "forwarded", "content-length", "transfer-encoding", "connection", "upgrade", "te", "trailer", "keep-alive", "x-original-url", "x-rewrite-url", "x-http-method", "x-method-override", "x-http-method-override", "origin", "referer", "expect", "via", "set-cookie", "accept-encoding":
						return invalid("forbidden transfer header")
					}
				}
			}
			if raw := t["form"]; raw != nil {
				data, _ := json.Marshal(raw)
				dec := json.NewDecoder(bytes.NewReader(data))
				dec.DisallowUnknownFields()
				if f.Method != "POST" || dec.Decode(&f.Form) != nil || f.Form == nil || f.Form.Fields == nil || !formName(f.Form.FileField) {
					return invalid("invalid upload form")
				}
				for _, field := range f.Form.Fields {
					if !formName(field.Name) || field.Name == f.Form.FileField || strings.ContainsAny(field.Value, "\r\n\x00") {
						return invalid("invalid upload form field")
					}
				}
				for key := range f.Headers {
					switch strings.ToLower(key) {
					case "content-type", "content-encoding", "content-range", "content-digest", "repr-digest":
						return invalid("incompatible form header")
					}
				}

			}
			if t["credentials"] == "omit" && address != "" {
				u, err := url.Parse(address)
				if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
					return invalid("invalid signed file URL")
				}
				continue // Storage is responsible for these bytes and signed headers.
			}
			b.mu.RLock()
			r := b.live[h.Source]
			var s Source
			if r != nil {
				s = r.transportSource
			}
			b.mu.RUnlock()
			if s.Type != "streamable-http" && s.Type != "sse" {
				return invalid("stdio upstream must use a signed HTTPS file URL")
			}
			u, err := fileOriginURL(s.URL, address, path)
			if err != nil {
				return err
			}
			if *f.File.Size > 1<<40 {
				return invalid("file exceeds relay size limit")
			}
			f.URL = u.String()
			public := ctx.Value(localConnectionKey{}) == true
			scope := p.Name
			if public {
				scope = ""
			}
			token, err := b.store.Seal("file-transfer", scope, f)
			if err != nil {
				return err
			}
			key := base64.RawURLEncoding.EncodeToString(token)
			delete(t, "headers")
			delete(t, "form")
			delete(t, "path")
			delete(t, "url")
			if public {
				base, err := url.Parse(os.Getenv("BRIDGE_PUBLIC_URL"))
				if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
					return errors.New("configure BRIDGE_PUBLIC_URL for HTTP file targets used by stdio clients")
				}
				t["url"] = strings.TrimRight(base.String(), "/") + "/mcp-file-link/" + key
				t["credentials"] = "omit"
			} else {
				t["path"] = "/files/" + key
				t["credentials"] = "include"
			}
			t["expiresAt"] = time.Unix(f.Expires, 0).UTC().Format(time.RFC3339)
		}
	}
	return nil
}

func formName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\"\\") && strings.IndexFunc(name, unicode.IsControl) < 0
}

// Keep transfer bytes out of MCP messages. Verify before signalling success to
// either party. Temporary files live in a private, daemon-owned directory.
func (b *Bridge) stageBytes(body io.Reader, size int64, info fileInfo) (*os.File, error) {
	tmp, err := os.CreateTemp(filepath.Join(b.dir, "transfers"), "body-")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(body, size+1))
	if err == nil && n != size {
		err = errors.New("unexpected transfer length")
	}
	if err == nil && info.Digest != nil && base64.RawURLEncoding.EncodeToString(hash.Sum(nil)) != info.Digest.Value {
		err = errors.New("file digest mismatch")
	}
	if err == nil {
		_, err = tmp.Seek(0, 0)
	}
	if err != nil {
		discardBody(tmp)
		return nil, err
	}
	return tmp, nil
}
func discardBody(f *os.File) { f.Close(); os.Remove(f.Name()) }

func formBody(f fileTicket, body *os.File) (io.Reader, int64, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, field := range f.Form.Fields {
		if err := w.WriteField(field.Name, field.Value); err != nil {
			return nil, 0, "", err
		}
	}
	h := textproto.MIMEHeader{}
	name := strings.ReplaceAll(f.File.Name, "\\", "/")
	name = name[strings.LastIndexByte(name, '/')+1:]
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	name = strings.NewReplacer("\r", "%0D", "\n", "%0A", "\"", "%22").Replace(name)
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, 0, "", errors.New("invalid filename")
	}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, f.Form.FileField, name))
	typ := f.File.MIME
	if _, _, err := mime.ParseMediaType(typ); err != nil || strings.IndexFunc(typ, unicode.IsControl) >= 0 {
		typ = "application/octet-stream"
	}
	h.Set("Content-Type", typ)
	if _, err := w.CreatePart(h); err != nil {
		return nil, 0, "", err
	}
	split := buf.Len()
	if err := w.Close(); err != nil {
		return nil, 0, "", err
	}
	return io.MultiReader(bytes.NewReader(buf.Bytes()[:split]), body, bytes.NewReader(buf.Bytes()[split:])), int64(buf.Len()) + *f.File.Size, w.FormDataContentType(), nil
}

func (b *Bridge) transfer(w http.ResponseWriter, r *http.Request, scope string) {
	var f fileTicket
	raw, err := base64.RawURLEncoding.DecodeString(r.PathValue("token"))
	if err != nil || b.store.Decode("file-transfer", scope, raw, &f) != nil || (scope != "" && f.Actor != scope) || time.Now().Unix() >= f.Expires || r.Method != f.Method {
		http.Error(w, "invalid transfer", 403)
		return
	}
	backend, h, err := b.relayTarget(Principal{f.Actor}, f.Source, f.Tool)
	if err != nil || h.Origin != f.Origin || (r.PathValue("source") != "" && r.PathValue("source") != f.Source) {
		http.Error(w, "transfer unavailable", 403)
		return
	}
	src := backend.transportSource
	var reader io.Reader
	var length int64
	var contentType string
	if f.Method != "GET" {
		if r.ContentLength >= 0 && r.ContentLength != *f.File.Size {
			http.Error(w, "unexpected file length", 400)
			return
		}
		tmp, err := b.stageBytes(r.Body, *f.File.Size, f.File)
		if err != nil {
			http.Error(w, "invalid upload bytes", 400)
			return
		}
		defer discardBody(tmp)
		reader, length = tmp, *f.File.Size
		if f.Form != nil {
			reader, length, contentType, err = formBody(f, tmp)
			if err != nil {
				http.Error(w, "invalid upload form", 400)
				return
			}
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, f.URL, reader)
	if err != nil {
		http.Error(w, "invalid target", 400)
		return
	}
	req.ContentLength = length
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if f.Method == "GET" && r.Header.Get("Range") != "" {
		req.Header.Set("Range", r.Header.Get("Range"))
	}
	req.Header.Set("Accept-Encoding", "identity")
	if f.Auth {
		if err := backend.oauth.attach(req); err != nil {
			http.Error(w, "source authorization unavailable", 502)
			return
		}
		for k, v := range src.Headers {
			if strings.EqualFold(k, "Authorization") {
				req.Header.Set(k, v)
			}
		}
	}
	res, err := transferClient.Do(req)
	if err != nil {
		http.Error(w, "file transfer failed", 502)
		return
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		http.Error(w, "upstream transfer failed", 502)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if f.Method != "GET" {
		w.WriteHeader(res.StatusCode)
		return
	}
	var contents io.Reader = res.Body
	encoding := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		if res.StatusCode == 206 {
			http.Error(w, "unsupported range encoding", 502)
			return
		}
		var decoded io.ReadCloser
		switch encoding {
		case "gzip":
			decoded, err = gzip.NewReader(res.Body)
		case "deflate":
			decoded, err = zlib.NewReader(res.Body)
		default:
			err = errors.New("unexpected content encoding")
		}
		if err != nil {
			http.Error(w, "invalid download encoding", 502)
			return
		}
		defer decoded.Close()
		contents = decoded
	}
	if res.StatusCode == 206 && req.Header.Get("Range") != "" {
		for _, k := range []string{"Content-Type", "Content-Range", "Content-Length", "Accept-Ranges"} {
			if value := res.Header.Get(k); value != "" {
				w.Header().Set(k, value)
			}
		}
		w.WriteHeader(206)
		if _, err := io.Copy(w, res.Body); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	body, err := b.stageBytes(contents, *f.File.Size, f.File)
	if err != nil {
		http.Error(w, "invalid download bytes", 502)
		return
	}
	defer discardBody(body)
	w.Header().Set("Content-Length", fmt.Sprint(*f.File.Size))
	w.Header().Set("Content-Type", f.File.MIME)
	if _, err := io.Copy(w, body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (b *Bridge) prepareTransfers() error {
	dir := filepath.Join(b.dir, "transfers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("expected private transfer directory")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Type().IsRegular() && strings.HasPrefix(f.Name(), "body-") {
			if err = os.Remove(filepath.Join(dir, f.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
