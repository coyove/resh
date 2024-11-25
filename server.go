// Copyright 2018 Joshua J Baker and 2023 Coyove Zhang. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build darwin || netbsd || freebsd || openbsd || dragonfly || linux
// +build darwin netbsd freebsd openbsd dragonfly linux

package resh

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coyove/resh/internal"

	reuseport "github.com/kavu/go_reuseport"
)

var (
	ShortWriteEmitter = func() {}
	WriteRaceEmitter  = func(int) {}
	RequestMaxBytes   = 1 * 1024 * 1024
	TCPKeepAlive      = 60
	DebugFlag         = os.Getenv("RESH_DEBUG") != ""
)

func Listen(reuse bool, addr string) (*Listener, error) {
	var ln Listener
	var err error
	if reuse {
		ln.raw, err = reuseport.Listen("tcp", addr)
	} else {
		ln.raw, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	ln.f, err = ln.raw.(*net.TCPListener).File()
	if err != nil {
		ln.Close()
		return nil, err
	}
	ln.fd = int(ln.f.Fd())
	ln.fdhead = &Conn{}
	ln.fdtail = &Conn{}
	ln.fdhead.next = ln.fdtail
	ln.fdtail.prev = ln.fdhead
	return &ln, nil
}

type Listener struct {
	raw     net.Listener
	f       *os.File
	fd      int
	poll    *internal.Poll // epoll or kqueue
	buffer  []byte         // read packet buffer
	count   int32          // connection count
	fdconns map[int]*Conn  // loop connections fd -> conn
	fdhead  *Conn
	fdtail  *Conn
	sslCtx  *SSLCtx

	OnRedis   func(*Redis) (more bool)
	OnHTTP    func(*HTTP) (more bool)
	OnWSData  func(*Websocket, []byte)
	OnWSClose func(*Websocket, []byte)
	OnFdCount func(int)
	OnError   func(Error)
	Timeout   time.Duration
}

func (ln *Listener) Addr() net.Addr {
	return ln.raw.Addr()
}

func (ln *Listener) Count() int {
	return int(ln.count)
}

func (ln *Listener) LoadCertPEMs(cert, key []byte) error {
	runtime.LockOSThread()
	ctx, err := sslNewCtx(cert, key)
	if err != nil {
		return err
	}
	ln.sslCtx = ctx
	return nil
}

func (ln *Listener) Serve() {
	if ln.OnError == nil {
		panic("missing OnError handler")
	}
	if ln.OnRedis == nil {
		ln.OnRedis = func(c *Redis) bool {
			c.WriteError("OnRedis handler not found")
			return true
		}
	}
	if ln.OnHTTP == nil {
		ln.OnHTTP = func(c *HTTP) bool {
			c.Text(500, "OnHTTP handler not found")
			return true
		}
	}
	if ln.OnFdCount == nil {
		ln.OnFdCount = func(int) {}
	}

	ln.poll = internal.OpenPoll()
	ln.buffer = make([]byte, 0xFFFF)
	ln.fdconns = make(map[int]*Conn)
	ln.poll.AddRead(ln.fd)

	defer func() {
		if r := recover(); r != nil {
			ln.OnError(Error{Type: "panic", Cause: fmt.Errorf("fatal error %v: %s", r, debug.Stack())})
		}

		for _, c := range ln.fdconns {
			ln.closeConnWithError(c, "", nil)
		}
		ln.poll.Close()
		//println("-- server stopped")
	}()

	//fmt.Println("-- loop started --", l.idx)

	ln.poll.Wait(func(fd int, ev uint32) error {
		if fd == ln.fd {
			nfd, sa, err := syscall.Accept(fd)
			if err != nil {
				if err == syscall.EAGAIN {
					return nil
				}
				ln.OnError(Error{Type: "accept", Cause: err})
				return nil
			}
			if err := syscall.SetNonblock(nfd, true); err != nil {
				ln.OnError(Error{Type: "setnonblock", Cause: err})
				return nil
			}

			c := &Conn{fd: nfd, sa: sa, ln: ln}
			if ln.sslCtx != nil {
				ssl, err := ln.sslCtx.accept(nfd)
				if err != nil {
					ln.OnError(Error{Type: "ssl", Cause: err})
					return nil
				}
				c.ssl = ssl
			}
			internal.SetKeepAlive(c.fd, TCPKeepAlive)

			ln.poll.AddRead(c.fd)
			ln.fdconns[c.fd] = c
			ln.attachConn(c)
			ln.OnFdCount(int(atomic.AddInt32(&ln.count, 1)))

			if DebugFlag {
				fmt.Printf("[%d] accept fd %d\n", ln.count, c.fd)
			}
		} else {
			c, ok := ln.fdconns[fd]
			if !ok {
				if DebugFlag {
					// The fd has already been closed, concurrent writes or reads are not possible anymore.
					// This is really not a big deal because protocols at application layer can detect and handle it.
					ln.OnError(Error{Type: "lookup", Cause: fmt.Errorf("fd %d not found", fd)})
				}
				return nil
			}
			ln.attachConn(c.detach())

			if ev&internal.WRITE > 0 {
				ln.writeConn(c)
			}
			if ev&internal.READ > 0 {
				ln.readConn(c)
			}
			if ev&internal.EOF > 0 {
				ln.closeConnWithError(c, "eof", nil)
			}
		}

		if ln.Timeout > 0 {
			for conn, now := ln.fdtail.prev, time.Now().UnixNano(); conn != nil && conn != ln.fdhead; conn = conn.prev {
				if conn.ts < now-int64(ln.Timeout) {
					ln.closeConnWithError(conn, "timeout", fmt.Errorf("connection to %v timed out (fd=%d)", conn.RemoteAddr(), conn.fd))
				} else {
					break
				}
			}
		}
		return nil
	})
}

func (ln *Listener) attachConn(c *Conn) {
	ln.fdhead.next.prev = c
	c.next = ln.fdhead.next

	ln.fdhead.next = c
	c.prev = ln.fdhead

	c.ts = time.Now().UnixNano()
}

func (ln *Listener) closeConnWithError(c *Conn, errType string, err error) {
	if !c.closed.CompareAndSwap(0, 1) {
		return
	}

	if c.ws != nil {
		ln.OnWSClose(c.ws, c.ws.closingData)
	}

	ln.OnFdCount(int(atomic.AddInt32(&ln.count, -1)))
	c.detach()
	delete(ln.fdconns, c.fd)

	if DebugFlag {
		_, fn, line, _ := runtime.Caller(1)
		fmt.Printf("[%d] close fd %d: %v at %s:%d\n", ln.count, c.fd, err, fn, line)
	}

	if err := syscall.Close(c.fd); err != nil {
		ln.OnError(Error{Type: "close", Cause: err})
	}
	if err != nil {
		if e, ok := err.(Error); ok {
			ln.OnError(e)
		} else {
			ln.OnError(Error{Type: errType, Cause: err})
		}
	}
	if c.ssl != nil {
		c.ssl.Close()
	}
}

func (ln *Listener) writeConn(c *Conn) int {
	c.spinLock()
	if len(c.out) == 0 {
		c.spinUnlock()
		ln.poll.ModRead(c.fd)
		return 1
	}

	var n int
	var err error
	if c.ssl != nil {
		n, err = c.ssl.Write(c.out)
	} else {
		n, err = syscall.Write(c.fd, c.out)
	}
	if err != nil {
		if err == syscall.EAGAIN {
			if n > 0 {
				c.out = c.out[n:]
			}
			c.spinUnlock()
			ln.poll.ModReadWrite(c.fd)
			return -n
		}
		c.spinUnlock()
		ln.closeConnWithError(c, "write", err)
		return 1
	}

	if n == len(c.out) {
		c.out = c.out[:0]
		c.spinUnlock()

		if c.ws != nil && c.ws.closed {
			ln.closeConnWithError(c, "", nil)
		} else {
			ln.poll.ModRead(c.fd)
		}
		return 1
	}

	c.out = c.out[n:]
	c.spinUnlock()

	ln.poll.ModReadWrite(c.fd)
	ShortWriteEmitter()
	return -n
}

func (ln *Listener) readConn(c *Conn) {
	var n int
	var err error
	if c.ssl != nil {
		n, err = c.ssl.Read(ln.buffer)
	} else {
		n, err = syscall.Read(c.fd, ln.buffer)
	}
	if n == 0 {
		ln.closeConnWithError(c, "", nil)
		return
	}
	if err != nil {
		if err == syscall.EAGAIN {
			ln.poll.ModRead(c.fd)
			return
		}
		ln.closeConnWithError(c, "read", err)
		return
	}

PARSE_NEXT:
	c.spinLock()
	c.in = append(c.in, ln.buffer[:n]...)
	if len(c.in) > RequestMaxBytes {
		ln.closeConnWithError(c, "oversize", fmt.Errorf("request too large: %db", len(c.in)))
		return
	}
	if c.ws != nil {
		if c.ws.closed {
			err = errWaitMore
		} else {
			err = c.ws.parse(c.in)
		}
	} else {
		err = c.srs.process(c.in)
	}
	c.spinUnlock()

	if err == errWaitMore {
		return
	}

	if err != nil {
		ln.closeConnWithError(c, "read", err)
		return
	}

	if c.ws != nil {
		req := c.ws.parsedFrame
		remain := c.truncateInputBuffer(req.len)
		if !ln.onWebsocket(req, c) {
			// Conn already closed
			return
		}
		if remain > 0 {
			n = 0
			goto PARSE_NEXT
		}
	} else if c.srs.http != nil {
		req := c.srs.http
		req.Conn = c
		c.truncateInputBuffer(int(req.bodyLen) + int(req.hdrLen))
		if !ln.OnHTTP(req) {
			ln.closeConnWithError(c, "", nil)
			return
		}
	} else {
		req := c.srs.redis
		req.Conn = c
		c.truncateInputBuffer(int(req.read))
		if !ln.OnRedis(req) {
			ln.closeConnWithError(c, "", nil)
			return
		}
	}
	c.srs = serverReadState{}

	if len(c.out) != 0 {
		ln.writeConn(c)
	}
}

func (ln *Listener) Close() {
	if ln.fd != 0 {
		syscall.Close(ln.fd)
	}
	if ln.f != nil {
		ln.f.Close()
	}
	if ln.raw != nil {
		ln.raw.Close()
	}
	if ln.sslCtx != nil {
		ln.sslCtx.close()
		runtime.UnlockOSThread()
	}
}
