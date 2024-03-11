//go:build darwin || netbsd || freebsd || openbsd || dragonfly || linux
// +build darwin netbsd freebsd openbsd dragonfly linux

package redis

import (
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coyove/resh"
	"github.com/coyove/resh/internal"
)

var ResponseMaxBytes = 1 * 1024 * 1024

type Client struct {
	poll   *internal.Poll
	auth   string
	addr   syscall.Sockaddr
	buffer []byte
	sslCtx *resh.SSLCtx

	fdActive fdMap[Conn]
	fdIdle   chan *Conn

	OnFdCount func()
	OnError   func(resh.Error)
	Timeout   time.Duration
}

func NewClient(poolSize int, auth, addr string) (*Client, error) {
	taddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	d := &Client{}

	if len(taddr.IP) == 4 {
		d.addr = &syscall.SockaddrInet4{Port: taddr.Port}
		copy(d.addr.(*syscall.SockaddrInet4).Addr[:], taddr.IP)
	} else {
		d.addr = &syscall.SockaddrInet6{Port: taddr.Port}
		copy(d.addr.(*syscall.SockaddrInet6).Addr[:], taddr.IP)
	}

	d.auth = auth
	d.poll = internal.OpenPoll()
	d.buffer = make([]byte, 0xFFFF)
	d.fdIdle = make(chan *Conn, poolSize)
	d.fdActive.fds.Store(new([]atomic.Pointer[Conn]))
	d.OnFdCount = func() {}

	go func() {
		runtime.LockOSThread()
		// ctx, err := sslNewClientCtx()
		// if err != nil {
		// 	return nil, err
		// }
		// d.sslCtx = ctx

		defer func() {
			if r := recover(); r != nil {
				d.OnError(resh.Error{Type: "panic", Cause: fmt.Errorf("fatal error %v: %s", r, debug.Stack())})
			}
			runtime.UnlockOSThread()
		}()

		d.poll.Wait(func(fd int, ev uint32) error {
			c := d.fdActive.Get(fd)
			if c == nil {
				d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("fd %d not found", fd)})
				return nil
			}

			// if c.callback != nil {
			// 	d.attachConn(d.fdActive.head, c.detach())
			// }
			// if d.Timeout > 0 {
			// 	d.fdActive.tail.lock()
			// 	conn, now := d.fdActive.tail.prev, time.Now().UnixNano()
			// 	d.fdActive.tail.unlock()

			// 	for ; conn != nil && conn != d.fdActive.head; conn = conn.prev {
			// 		if conn.ts < now-int64(d.Timeout) {
			// 			d.closeConnWithError(conn, false, "timeout", fmt.Errorf("connection timed out (fd=%d)", conn.fd))
			// 		} else {
			// 			break
			// 		}
			// 	}
			// }

			if ev&syscall.EPOLLOUT > 0 {
				if c.callback == nil {
					d.closeConnWithError(c, "write", fmt.Errorf("write: fd %d is not active", fd))
					return nil
				}
				d.writeConn(c)
			} else {
				d.readConn(c)
			}
			return nil
		})
	}()

	return d, nil
}

type Conn struct {
	ts       int64
	fd       int
	callback func(*Reader, error)
	ssl      *resh.SSL
	in       []byte
	out      []byte

	// 0: no auth (default)                <---------------------------+
	// 1: need auth, the first response will be treated as AUTH result |
	// 2: received AUTH result, waiting for the actual CMD response    |
	//    after receiving any response     ----------------------------+
	authState byte
}

func (d *Client) String() string {
	return fmt.Sprintf("(host=%v, idle=%d, active=%d, total=%d)",
		internal.SockaddrToAddr(d.addr), len(d.fdIdle), d.fdActive.Len(), d.fdActive.Len()+len(d.fdIdle))
}

func (s *Client) ActiveCount() int { return s.fdActive.Len() }

func (s *Client) IdleCount() int { return len(s.fdIdle) }

func (d *Client) Exec(cmdArgs []any, cb func(*Reader, error)) {
	if d.OnError == nil {
		panic("missing OnError handler")
	}

	d.activateFreeConn(cb, func(c *Conn) {
		c.out = append(c.out, '*')
		c.out = strconv.AppendInt(c.out, int64(len(cmdArgs)), 10)
		c.out = append(c.out, "\r\n"...)
		for _, a := range cmdArgs {
			var s string
			switch a := a.(type) {
			case []byte:
				s = btos(a)
			case string:
				s = a
			case int:
				s = strconv.Itoa(a)
			case int64:
				s = strconv.FormatInt(a, 10)
			default:
				s = fmt.Sprint(a)
			}
			c.out = append(c.out, '$')
			c.out = strconv.AppendInt(c.out, int64(len(s)), 10)
			c.out = append(c.out, "\r\n"...)
			c.out = append(c.out, s...)
			c.out = append(c.out, "\r\n"...)
		}
	})
}

func (d *Client) activateFreeConn(cb func(*Reader, error), cc func(*Conn)) {
AGAIN:
	select {
	case conn := <-d.fdIdle:
		if len(conn.out) > 0 {
			panic("BUG")
		}

		conn.callback = cb
		conn.out = conn.out[:0]
		cc(conn)
		d.fdActive.Add(conn.fd, conn)

		// Free idle connections if there are too many connections.
		for d.fdActive.Len()+len(d.fdIdle) > cap(d.fdIdle) {
			select {
			case conn := <-d.fdIdle:
				d.closeConnWithError(conn, "", nil)
				continue
			default:
			}
			break
		}

		d.OnFdCount()
		d.poll.Trigger(conn.fd)
		return
	default:
		if d.fdActive.Len()+len(d.fdIdle) >= cap(d.fdIdle) {
			goto AGAIN
		}
	}

	af := syscall.AF_INET
	if _, ok := d.addr.(*syscall.SockaddrInet6); ok {
		af = syscall.AF_INET6
	}
	fd, err := syscall.Socket(af, syscall.SOCK_STREAM, 0)
	if err != nil {
		cb(nil, err)
		return
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		cb(nil, err)
		return
	}
	if err := syscall.Connect(fd, d.addr); err != nil && err != syscall.EINPROGRESS {
		syscall.Close(fd)
		cb(nil, err)
		return
	}

	c := &Conn{
		fd:       fd,
		callback: cb,
	}
	if d.auth != "" {
		c.authState = 1
		c.out = append(c.out, "*2\r\n$4\r\nAUTH\r\n$"...)
		c.out = strconv.AppendInt(c.out, int64(len(d.auth)), 10)
		c.out = append(c.out, "\r\n"...)
		c.out = append(c.out, d.auth...)
		c.out = append(c.out, "\r\n"...)
	}
	cc(c)

	d.fdActive.Add(fd, c)
	d.OnFdCount()
	d.poll.AddReadWrite(fd)
}

func (d *Client) closeConnWithError(c *Conn, typ string, err error) {
	// _, _, line, _ := runtime.Caller(1)
	// fmt.Println("close", c.fd, err, line)

	if c.callback == nil {
		// Idle connections
	} else {
		d.fdActive.Delete(c.fd)
	}

	d.OnFdCount()

	if err := syscall.Close(c.fd); err != nil {
		d.OnError(resh.Error{Type: "close", Cause: err})
	}

	if err != nil {
		d.OnError(resh.Error{Type: typ, Cause: err})
	}

	if typ != "free" && c.callback != nil {
		if err == nil {
			c.callback(nil, net.ErrClosed)
		} else {
			c.callback(nil, err)
		}
	}

	if c.ssl != nil {
		c.ssl.Close()
	}
}

func (s *Client) writeConn(c *Conn) {
	if len(c.out) == 0 {
		s.poll.ModRead(c.fd)
		return
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
			s.poll.ModReadWrite(c.fd)
			return
		}
		s.closeConnWithError(c, "write", err)
		return
	}

	if n == len(c.out) {
		c.out = c.out[:0]
		s.poll.ModRead(c.fd)
	} else {
		c.out = c.out[n:]
		s.poll.ModReadWrite(c.fd)
	}
}

func (d *Client) readConn(c *Conn) {
	var n int
	var err error
	if c.ssl != nil {
		n, err = c.ssl.Read(d.buffer)
	} else {
		n, err = syscall.Read(c.fd, d.buffer)
	}
	if n == 0 {
		d.closeConnWithError(c, "", nil)
		return
	}
	if err != nil {
		if err == syscall.EAGAIN {
			d.poll.ModRead(c.fd)
			return
		}
		d.closeConnWithError(c, "read", err)
		return
	}

	if c.callback == nil {
		d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("read: fd %d is not active", c.fd)})
		return
	}

	c.in = append(c.in, d.buffer[:n]...)
	if len(c.in) > ResponseMaxBytes {
		d.closeConnWithError(c, "oversize", fmt.Errorf("response too large: %db", len(c.in)))
		return
	}

	for len(c.in) > 0 {
		bb, err := readElement(c.in)
		if err == errWaitMore {
			break
		}
		if err != nil {
			d.closeConnWithError(c, "read", err)
			return
		}
		if c.authState == 1 {
			c.authState = 2
		} else {
			r := &Reader{buf: c.in[:bb]}
			c.callback(r, r.err())
			c.authState = 0
		}
		c.in = c.in[bb:]
	}

	if len(c.in) == 0 && c.authState == 0 {
		if d.fdActive.Len()+len(d.fdIdle) <= cap(d.fdIdle) {
			c.callback = nil
			d.fdActive.Delete(c.fd)
			select {
			case d.fdIdle <- c:
			default:
				d.closeConnWithError(c, "free", nil)
			}
		} else {
			d.closeConnWithError(c, "free", nil)
		}
	}
}

func (d *Client) Close() {
	if d.sslCtx != nil {
		// 	d.sslCtx.close()
	}
	d.fdActive.Foreach(func(fd int, c *Conn) {
		d.closeConnWithError(c, "", nil)
	})
	d.poll.Close()
}
