package resh

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"unsafe"
)

type Error struct {
	Cause error
	Type  string
}

func (e Error) Error() string {
	return e.Type + ": " + e.Cause.Error()
}

var errWaitMore = fmt.Errorf("wait more")

func readByteAndNumberCrLf(head byte, in []byte) (int64, int, error) {
	if len(in) < 4 { // 1b + 1b + '\r\n'
		return 0, 0, errWaitMore
	}
	if in[0] != head {
		return 0, 0, nil
	}
	idx := bytes.Index(in, []byte("\r\n"))
	if idx == -1 {
		return 0, 0, errWaitMore
	}
	v, err := strconv.ParseInt(btos(in[1:idx]), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return v, idx + 2, nil
}

func UnescapeInplace(s []byte, plusSpace bool) []byte {
	x := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%':
			if i < len(s)-2 {
				hex.Decode(s[x:x+1], s[i+1:i+3])
				i += 2
			}
		case '+':
			if plusSpace {
				s[x] = ' '
			} else {
				s[x] = '+'
			}
		default:
			s[x] = s[i]
		}
		x++
	}
	return s[:x]
}

func btos(in []byte) string {
	return unsafe.String(unsafe.SliceData(in), len(in))
}

type netHTTPResponseWriter struct {
	code int
	h    http.Header
	w    bytes.Buffer
}

func (w *netHTTPResponseWriter) StatusCode() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *netHTTPResponseWriter) Header() http.Header {
	if w.h == nil {
		w.h = make(http.Header)
	}
	return w.h
}

func (w *netHTTPResponseWriter) WriteHeader(statusCode int) {
	w.code = statusCode
}

func (w *netHTTPResponseWriter) Write(p []byte) (int, error) {
	return w.w.Write(p)
}

func (w *netHTTPResponseWriter) Flush() {}
