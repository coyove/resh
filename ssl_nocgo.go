//go:build !cgo

package resh

import (
	"fmt"

	"github.com/coyove/resh/internal"
)

type SSLCtx struct{}

func sslNewCtx(cert, key string) (*SSLCtx, error) {
	return nil, fmt.Errorf("SSL support requires cgo and OpenSSL")
}

type SSL struct{}

func (ctx *SSLCtx) accept(poll *internal.Poll, fd int) (*SSL, error) {
	panic(0)
}

func (s *SSL) Read(p []byte) (int, error) {
	panic(0)
}

func (s *SSL) Write(p []byte) (int, error) {
	panic(0)
}

func (s *SSL) Close() {
	panic(0)
}
