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
	"strconv"
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

func (s *Listener) Addr() net.Addr {
	return s.raw.Addr()
}

func (s *Listener) Count() int {
	return int(s.count)
}

func (s *Listener) LoadCertPEMs(cert, key []byte) error {
	ctx, err := sslNewCtx(cert, key)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	s.sslCtx = ctx
	return nil
}

type Conn struct {
	Tag any

	prev *Conn
	next *Conn
	ts   int64

	ws  *Websocket
	fd  int
	srs serverReadState
	sa  syscall.Sockaddr
	ln  *Listener
	ssl *SSL

	closed atomic.Int32

	// lock protects the following fields
	lock atomic.Int32
	in   []byte
	out  []byte
}

func (c *Conn) spinLock() {
	for i := 0; !c.lock.CompareAndSwap(0, 1); i++ {
		WriteRaceEmitter(i)
	}
}

func (c *Conn) spinUnlock() {
	c.lock.Store(0)
}

func (c *Conn) RemoteAddr() net.Addr {
	return internal.SockaddrToAddr(c.sa)
}

func (c *Conn) Write(p []byte) (int, error) {
	c.spinLock()
	c.out = append(c.out, p...)
	c.spinUnlock()
	return len(p), nil
}

func (c *Conn) _writeInt(v int64, b int) {
	c.spinLock()
	c.out = strconv.AppendInt(c.out, v, b)
	c.spinUnlock()
}

func (c *Conn) _writeString(v string) {
	c.spinLock()
	c.out = append(c.out, v...)
	c.spinUnlock()
}

func (c *Conn) Flush() {
	c.ln.poll.Trigger(c.fd)
}

func (c *Conn) truncateInputBuffer(sz int) int {
	c.spinLock()
	c.in = c.in[sz:]
	remain := len(c.in)
	c.spinUnlock()
	return remain
}

func (c *Conn) ReuseInputBuffer(in []byte) {
	c.spinLock()
	if len(c.in) == 0 {
		c.in = in[:0]
	}
	c.spinUnlock()
}

func (c *Conn) detach() *Conn {
	c.prev.next = c.next
	c.next.prev = c.prev
	return c
}

func (s *Listener) Serve() {
	if s.OnError == nil {
		panic("missing OnError handler")
	}
	if s.OnRedis == nil {
		s.OnRedis = func(c *Redis) bool {
			c.WriteError("OnRedis handler not found")
			return true
		}
	}
	if s.OnHTTP == nil {
		s.OnHTTP = func(c *HTTP) bool {
			c.Text(500, "OnHTTP handler not found")
			return true
		}
	}
	if s.OnFdCount == nil {
		s.OnFdCount = func(int) {}
	}

	s.poll = internal.OpenPoll()
	s.buffer = make([]byte, 0xFFFF)
	s.fdconns = make(map[int]*Conn)
	s.poll.AddRead(s.fd)

	defer func() {
		if r := recover(); r != nil {
			s.OnError(Error{Type: "panic", Cause: fmt.Errorf("fatal error %v: %s", r, debug.Stack())})
		}

		for _, c := range s.fdconns {
			s.closeConnWithError(c, "", nil)
		}
		s.poll.Close()
		//println("-- server stopped")
	}()

	//fmt.Println("-- loop started --", l.idx)

	s.poll.Wait(func(fd int, ev uint32) error {
		if fd == s.fd {
			nfd, sa, err := syscall.Accept(fd)
			if err != nil {
				if err == syscall.EAGAIN {
					return nil
				}
				s.OnError(Error{Type: "accept", Cause: err})
				return nil
			}
			if err := syscall.SetNonblock(nfd, true); err != nil {
				s.OnError(Error{Type: "setnonblock", Cause: err})
				return nil
			}

			c := &Conn{fd: nfd, sa: sa, ln: s}
			if s.sslCtx != nil {
				ssl, err := s.sslCtx.accept(nfd)
				if err != nil {
					s.OnError(Error{Type: "ssl", Cause: err})
					return nil
				}
				c.ssl = ssl
			}
			internal.SetKeepAlive(c.fd, TCPKeepAlive)

			s.poll.AddRead(c.fd)
			s.fdconns[c.fd] = c
			s.attachConn(c)
			s.OnFdCount(int(atomic.AddInt32(&s.count, 1)))
		} else {
			c, ok := s.fdconns[fd]
			if !ok {
				s.OnError(Error{Type: "lookup", Cause: fmt.Errorf("fd %d not found", fd)})
				return nil
			}

			s.attachConn(c.detach())
			if ev&internal.WRITE > 0 {
				s.writeConn(c)
			}
			if ev&internal.READ > 0 {
				s.readConn(c)
			}
			if ev&internal.EOF > 0 {
				s.closeConnWithError(c, "eof", nil)
			}
		}

		if s.Timeout > 0 {
			for conn, now := s.fdtail.prev, time.Now().UnixNano(); conn != nil && conn != s.fdhead; conn = conn.prev {
				if conn.ts < now-int64(s.Timeout) {
					s.closeConnWithError(conn, "timeout", fmt.Errorf("connection to %v timed out (fd=%d)", conn.RemoteAddr(), conn.fd))
				} else {
					break
				}
			}
		}
		return nil
	})
}

func (s *Listener) attachConn(c *Conn) {
	s.fdhead.next.prev = c
	c.next = s.fdhead.next

	s.fdhead.next = c
	c.prev = s.fdhead

	c.ts = time.Now().UnixNano()
}

func (s *Listener) closeConnWithError(c *Conn, typ string, err error) {
	if !c.closed.CompareAndSwap(0, 1) {
		return
	}

	if c.ws != nil {
		s.OnWSClose(c.ws, c.ws.closingData)
	}
	// _, _, line, _ := runtime.Caller(1)
	// fmt.Println("close", c.fd, err, line)
	s.OnFdCount(int(atomic.AddInt32(&s.count, -1)))
	c.detach()
	delete(s.fdconns, c.fd)

	if err := syscall.Close(c.fd); err != nil {
		s.OnError(Error{Type: "close", Cause: err})
	}
	if err != nil {
		s.OnError(Error{Type: typ, Cause: err})
	}
	if c.ssl != nil {
		c.ssl.Close()
	}
}

func (s *Listener) writeConn(c *Conn) int {
	c.spinLock()
	if len(c.out) == 0 {
		c.spinUnlock()
		s.poll.ModRead(c.fd)
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
			s.poll.ModReadWrite(c.fd)
			return -n
		}
		c.spinUnlock()
		s.closeConnWithError(c, "write", err)
		return 1
	}

	if n == len(c.out) {
		c.out = c.out[:0]
		c.spinUnlock()

		if c.ws != nil && c.ws.closed {
			s.closeConnWithError(c, "", nil)
		} else {
			s.poll.ModRead(c.fd)
		}
		return 1
	}

	c.out = c.out[n:]
	c.spinUnlock()

	s.poll.ModReadWrite(c.fd)
	ShortWriteEmitter()
	return -n
}

func (s *Listener) readConn(c *Conn) {
	var n int
	var err error
	if c.ssl != nil {
		n, err = c.ssl.Read(s.buffer)
	} else {
		n, err = syscall.Read(c.fd, s.buffer)
	}
	if n == 0 {
		s.closeConnWithError(c, "", nil)
		return
	}
	if err != nil {
		if err == syscall.EAGAIN {
			s.poll.ModRead(c.fd)
			return
		}
		s.closeConnWithError(c, "read", err)
		return
	}

PARSE_NEXT:
	c.spinLock()
	c.in = append(c.in, s.buffer[:n]...)
	if len(c.in) > RequestMaxBytes {
		s.closeConnWithError(c, "oversize", fmt.Errorf("request too large: %db", len(c.in)))
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
		s.closeConnWithError(c, "read", err)
		return
	}

	if c.ws != nil {
		req := c.ws.parsedFrame
		remain := c.truncateInputBuffer(req.len)
		switch req.opcode {
		case 0: // continuation
			if c.ws.contFrame == nil {
				s.closeConnWithError(c, "websocket", fmt.Errorf("unexpected continuation frame"))
				return
			}
			c.ws.contFrame = append(c.ws.contFrame, req.data...)
			if len(c.ws.contFrame) > RequestMaxBytes {
				s.closeConnWithError(c, "websocket", fmt.Errorf("continuation frame too large"))
				return
			}
			if req.fin {
				s.OnWSData(c.ws, c.ws.contFrame)
				c.ws.contFrame = nil
			}
		case 8: // close
			c.ws.closingData = req.data
			s.closeConnWithError(c, "", nil)
			return
		case 9: // ping
			c.ws.write(10, btos(req.data))
		case 10: // pong
		default:
			if req.fin {
				s.OnWSData(c.ws, req.data)
			} else {
				c.ws.contFrame = append([]byte{}, req.data...)
			}
		}
		if remain > 0 {
			n = 0
			goto PARSE_NEXT
		}
	} else if c.srs.http != nil {
		req := c.srs.http
		req.Conn = c
		c.truncateInputBuffer(int(req.bodyLen) + int(req.hdrLen))
		if !s.OnHTTP(req) {
			s.closeConnWithError(c, "", nil)
			return
		}
	} else {
		req := c.srs.redis
		req.Conn = c
		c.truncateInputBuffer(int(req.read))
		if !s.OnRedis(req) {
			s.closeConnWithError(c, "", nil)
			return
		}
	}
	c.srs = serverReadState{}

	if len(c.out) != 0 {
		s.writeConn(c)
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
