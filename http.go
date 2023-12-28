package resh

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/pprof"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"github.com/coyove/sdss/contrib/plru"
)

var crlf = []byte("\r\n")

type HTTP struct {
	Conn      *Conn
	Host      string
	Path      string
	qmap      *plru.Map[string, string]
	data      []byte
	qStart    uint16
	qEnd      uint16
	hdrLen    uint32
	bodyLen   uint32
	wsUpgrade bool
	chunked   bool
	chkbuf    []byte
}

func (r *HTTP) Method() string {
	method := r.data[:bytes.IndexByte(r.data, ' ')]
	for i, c := range method {
		if 'a' <= c && c <= 'z' {
			method[i] = c - 'a' + 'A'
		}
	}
	return btos(method)
}

func (r *HTTP) Body() []byte {
	return r.data[len(r.data)-int(r.bodyLen):]
}

func (r *HTTP) parse() error {
	r.hdrLen = uint32(len(r.data))
	for start := 0; start < len(r.data); {
		idx := bytes.Index(r.data[start:], crlf)
		if idx == 0 {
			break
		}
		line := r.data[start : start+idx]

		if start == 0 {
			idx0, idx1 := bytes.IndexByte(line, ' '), bytes.LastIndexByte(line, ' ') // <Method><WS><Path><WS><Version>
			if idx0 == -1 || idx1 == -1 || idx0 == idx1 || idx1 > 0xffff {
				return fmt.Errorf("invalid HTTP/1 first line: %q", line)
			}

			uri := line[idx0+1 : idx1]
			if q := bytes.IndexByte(uri, '?'); q >= 0 {
				r.qStart = uint16(idx0 + 1 + q + 1)
				r.qEnd = uint16(idx1)
				uri = uri[:q]
			}

			r.Path = btos(UnescapeInplace(uri, false))
			if !strings.HasPrefix(r.Path, "/") {
				u, err := url.Parse(r.Path)
				if err != nil {
					return fmt.Errorf("invalid HTTP/1 path %q: %v", line, err)
				}
				r.Path, r.Host = u.Path, u.Host
			}
		} else {
			idx := bytes.IndexByte(line, ':')
			if idx < 1 {
				return fmt.Errorf("invalid HTTP/1 header: %q", line)
			}
			key := line[:idx]
			for i, c := range key {
				if c == ':' {
					break
				}
				if 'A' <= c && c <= 'Z' {
					key[i] = c - 'A' + 'a'
				}
			}
			value := bytes.TrimSpace(line[idx+1:])
			switch btos(key) {
			case "upgrade":
				r.wsUpgrade = strings.EqualFold(btos(value), "websocket")
			case "host":
				r.Host = btos(value)
			case "content-length":
				cl, _ := strconv.Atoi(btos(value))
				r.bodyLen = uint32(cl)
			}
		}
		start += idx + 2
	}
	return nil
}

func (r *HTTP) URL() *url.URL {
	u := &url.URL{
		Scheme: "http:",
		Host:   r.Host,
		Path:   r.Path,
	}
	r.ForeachQuery(func(k string, v string) {
		u.RawQuery += url.QueryEscape(k) + "=" + url.QueryEscape(v) + "&"
	})
	return u
}

func (r *HTTP) ForeachHeader(f func(k, v string) bool) {
	for start := 0; start < len(r.data); {
		idx := bytes.Index(r.data[start:], crlf)
		if idx == 0 {
			break
		} else if start == 0 {
		} else {
			line := r.data[start : start+idx]
			if idx := bytes.IndexByte(line, ':'); idx > 0 {
				key, value := line[:idx], bytes.TrimSpace(line[idx+1:])
				if !f(btos(key), btos(value)) {
					return
				}
			}
		}
		start += idx + 2
	}
}

func (r *HTTP) GetHeader(key string) (value string) {
	r.ForeachHeader(func(k, v string) bool {
		if k == key {
			value = v
			return false
		}
		return true
	})
	return
}

func (r *HTTP) ForeachQuery(f func(k string, v string)) {
	if r.qStart == 0 {
		return
	}
	for query := r.data[r.qStart:r.qEnd]; len(query) > 0; {
		var part []byte
		part, query, _ = bytes.Cut(query, []byte("&"))
		key, value, _ := bytes.Cut(part, []byte("="))
		f(btos(UnescapeInplace(key, true)), btos(UnescapeInplace(value, true)))
	}
}

func (r *HTTP) Query() *plru.Map[string, string] {
	if r.qmap != nil {
		return r.qmap
	}
	if r.qStart > 0 {
		r.qmap = plru.NewMap[string, string](4, plru.Hash.Str)
		r.ForeachQuery(func(k string, v string) { r.qmap.Set(k, v) })
		return r.qmap
	}
	return nil
}

func (r *HTTP) GetQuery(k string) string {
	return r.Query().Get(k)
}

func (r *HTTP) GetQueryInt64(k string) (int64, error) {
	return strconv.ParseInt(r.Query().Get(k), 10, 64)
}

func (r *HTTP) GetQueryInt64Default(k string, v int64) int64 {
	res, err := strconv.ParseInt(r.Query().Get(k), 10, 64)
	if err != nil {
		res = v
	}
	return res
}

func (r *HTTP) Flush() *HTTP {
	r.Conn.Flush()
	return r
}

func (r *HTTP) Redirect(code int, location string) *HTTP {
	r.Conn._writeString("HTTP/1.1 ")
	r.Conn._writeInt(int64(code), 10)
	r.Conn._writeString(" ")
	r.Conn._writeString(http.StatusText(code))
	r.Conn._writeString("\r\nLocation: ")
	r.Conn._writeString(location)
	r.Conn._writeString("\r\n\r\n")
	return r
}

func (r *HTTP) Text(code int, msg string) *HTTP {
	return r.respFull(code, "", nil, msg)
}

func (r *HTTP) Bytes(code int, contentType string, data []byte) *HTTP {
	return r.respFull(code, contentType, nil, btos(data))
}

func (r *HTTP) BytesHeaders(code int, contentType string, hdr http.Header, data []byte) *HTTP {
	return r.respFull(code, contentType, hdr, btos(data))
}

func (r *HTTP) resp0(code int, contentType string, hdr http.Header) {
	if code == 0 {
		code = 200
	}
	r.Conn._writeString("HTTP/1.1 ")
	r.Conn._writeInt(int64(code), 10)
	r.Conn._writeString(" ")
	r.Conn._writeString(http.StatusText(code))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	r.Conn._writeString("\r\nContent-Type: ")
	r.Conn._writeString(contentType)
	for k, v := range hdr {
		switch k {
		case "Content-Type", "Connection", "Content-Length", "Transfer-Encoding":
		default:
			r.Conn._writeString("\r\n")
			r.Conn._writeString(k)
			r.Conn._writeString(": ")
			r.Conn._writeString(v[0])
		}
	}
}

func (r *HTTP) respFull(code int, contentType string, hdr http.Header, data string) *HTTP {
	r.resp0(code, contentType, hdr)
	r.Conn._writeString("\r\nConnection: Keep-Alive\r\nContent-Length: ")
	r.Conn._writeInt(int64(len(data)), 10)
	r.Conn._writeString("\r\n\r\n")
	r.Conn._writeString(data)
	return r
}

func (r *HTTP) StartChunked(code int, contentType string, hdr http.Header) {
	r.resp0(code, contentType, hdr)
	r.Conn._writeString("\r\nConnection: Keep-Alive\r\nTransfer-Encoding: chunked\r\n\r\n")
	r.chunked = true
}

func (w *HTTP) WriteString(s string) (int, error) {
	if len(s) == 0 {
		return 0, nil
	}
	n, err := w.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
	runtime.KeepAlive(s)
	return n, err
}

func (w *HTTP) Write(p []byte) (int, error) {
	if !w.chunked {
		panic("not in chunked mode, call StartChunked first")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) <= 4 || len(w.chkbuf) > 0 {
		w.chkbuf = append(w.chkbuf, p...)
		if len(w.chkbuf) >= 64 {
			w.writeChunked(w.chkbuf)
			w.chkbuf = w.chkbuf[:0]
		}
	} else {
		w.writeChunked(p)
	}
	return len(p), nil
}

func (w *HTTP) writeChunked(p []byte) {
	w.Conn._writeInt(int64(len(p)), 16)
	w.Conn._writeString("\r\n")
	w.Conn.Write(p)
	w.Conn._writeString("\r\n")
	if len(w.Conn.out) >= 16*1024 {
		w.Flush()
	}
}

func (w *HTTP) FinishChunked() {
	if !w.chunked {
		panic("not in chunked mode, call StartChunked first")
	}
	if len(w.chkbuf) > 0 {
		w.writeChunked(w.chkbuf)
		w.chkbuf = w.chkbuf[:0]
	}
	w.Conn._writeString("0\r\n\r\n")
	w.chunked = false
	w.Flush()
}

func (w *HTTP) UpgradeWebsocket(hdr http.Header) *Websocket {
	if !w.wsUpgrade {
		return nil
	}
	w.Conn.ws = &Websocket{Conn: w.Conn}
	key := w.GetHeader("sec-websocket-key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.Sum([]byte(key))
	w.Conn._writeString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	w.Conn._writeString(base64.StdEncoding.EncodeToString(h[:]))
	w.Conn._writeString("\r\n")
	for k, v := range hdr {
		switch k {
		case "Upgrade", "Connection", "Sec-WebSocket-Accept":
		default:
			w.Conn._writeString(k)
			w.Conn._writeString(": ")
			w.Conn._writeString(v[0])
			w.Conn._writeString("\r\n")
		}
	}
	w.Conn._writeString("\r\n")
	w.Conn.Flush()
	return w.Conn.ws
}

func RunPprof(sh *HTTP) {
	switch {
	case strings.HasPrefix(sh.Path, "/debug/pprof/cmdline"):
		sh.RunGoHandler(pprof.Cmdline)
	case strings.HasPrefix(sh.Path, "/debug/pprof/profile"):
		sh.RunGoHandler(pprof.Profile)
	case strings.HasPrefix(sh.Path, "/debug/pprof/symbol"):
		sh.RunGoHandler(pprof.Symbol)
	case strings.HasPrefix(sh.Path, "/debug/pprof/trace"):
		sh.RunGoHandler(pprof.Trace)
	default:
		sh.RunGoHandler(pprof.Index)
	}
}

func (sh *HTTP) RunGoHandler(h http.HandlerFunc) {
	r := http.Request{}
	r.URL = sh.URL()
	r.RequestURI = r.URL.String()
	r.Method = sh.Method()
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.Host = sh.Host
	r.Header = make(http.Header)

	sh.ForeachHeader(func(sk string, v string) bool {
		switch sk {
		case "transfer-encoding":
			r.TransferEncoding = append(r.TransferEncoding, v)
		default:
			r.Header.Set(sk, v)
		}
		return true
	})

	w := netHTTPResponseWriter{}
	h(&w, &r)
	sh.BytesHeaders(w.code, w.h.Get("Content-Type"), w.h, w.w.Bytes()).Flush()
}

func (r *HTTP) Release() {
	r.Conn.ReuseInputBuffer(r.data)
}
