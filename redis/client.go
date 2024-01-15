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

func NewClient(auth string, addr string) (*Client, error) {
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

	d.fdfree.head = &Conn{}
	d.fdfree.tail = &Conn{}
	d.fdfree.head.next = d.fdfree.tail
	d.fdfree.tail.prev = d.fdfree.head
	d.fdactive.head = &Conn{}
	d.fdactive.tail = &Conn{}
	d.fdactive.head.next = d.fdactive.tail
	d.fdactive.tail.prev = d.fdactive.head

	d.auth = auth
	d.poll = internal.OpenPoll()
	d.buffer = make([]byte, 0xFFFF)
	d.fdconns = make(map[int]*Conn)
	d.OnFdCount = func(int) {}

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
			d.fdSpinLock()
			c, ok := d.fdconns[fd]
			d.fdSpinUnlock()
			if !ok {
				d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("fd %d not found", fd)})
				return nil
			}

			d.fdSpinLock()
			if c.callback != nil {
				d.attachConn(d.fdactive.head, c.detach())
			}
			if d.Timeout > 0 {
				for conn, now := d.fdactive.tail.prev, time.Now().UnixNano(); conn != nil && conn != d.fdactive.head; conn = conn.prev {
					if conn.ts < now-int64(d.Timeout) {
						err := fmt.Errorf("connection timed out (fd=%d)", conn.fd)
						conn.callback(nil, err)
						d.closeConnWithError(conn, false, "timeout", err)
					} else {
						break
					}
				}
			}
			d.fdSpinUnlock()

			if ev&syscall.EPOLLOUT > 0 {
				if c.callback == nil {
					d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("write: fd %d is not active", fd)})
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

type Client struct {
	poll   *internal.Poll
	auth   string
	addr   syscall.Sockaddr
	buffer []byte
	count  int32
	sslCtx *resh.SSLCtx

	// lock protects the following fields
	fdlock   atomic.Int64
	fdconns  map[int]*Conn
	fdfree   struct{ head, tail *Conn }
	fdactive struct{ head, tail *Conn }

	OnFdCount func(int)
	OnError   func(resh.Error)
	Timeout   time.Duration
}

type Conn struct {
	prev      *Conn
	next      *Conn
	ts        int64
	fd        int
	callback  func(*Reader, error)
	ssl       *resh.SSL
	in        []byte
	out       []byte
	authState byte // 0: no auth/auth passed, 1: need auth, 2: AUTH responded
}

func (c *Conn) detach() *Conn {
	c.prev.next = c.next
	c.next.prev = c.prev
	return c
}

func (s *Client) Count() int {
	return int(s.count)
}

func (d *Client) Exec(args []any, cb func(*Reader, error)) error {
	if d.OnError == nil {
		panic("missing OnError handler")
	}

	return d.activateFreeConn(cb, func(c *Conn) {
		c.out = append(c.out, '*')
		c.out = strconv.AppendInt(c.out, int64(len(args)), 10)
		c.out = append(c.out, "\r\n"...)
		for _, a := range args {
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

func (d *Client) fdSpinLock() {
	for i := 0; !d.fdlock.CompareAndSwap(0, 1); i++ {
		runtime.Gosched()
	}
}

func (d *Client) fdSpinUnlock() {
	d.fdlock.Store(0)
}

func (d *Client) activateFreeConn(cb func(*Reader, error), cc func(*Conn)) error {
	d.fdSpinLock()
	for conn := d.fdfree.tail.prev; conn != nil && conn != d.fdfree.head; conn = conn.prev {
		if len(conn.out) > 0 {
			panic("BUG")
		}
		conn.callback = cb
		conn.out = conn.out[:0]
		cc(conn)
		d.attachConn(d.fdactive.head, conn.detach())
		d.fdSpinUnlock()
		d.poll.Trigger(conn.fd)
		return nil
	}
	d.fdSpinUnlock()

	af := syscall.AF_INET
	if _, ok := d.addr.(*syscall.SockaddrInet6); ok {
		af = syscall.AF_INET6
	}
	fd, err := syscall.Socket(af, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		return err
	}
	if err := syscall.Connect(fd, d.addr); err != nil && err != syscall.EINPROGRESS {
		return err
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
	d.fdSpinLock()
	d.fdconns[fd] = c
	d.count++
	d.attachConn(d.fdactive.head, c)
	d.poll.AddReadWrite(fd)
	d.fdSpinUnlock()
	return nil
}

func (s *Client) attachConn(head *Conn, c *Conn) {
	head.next.prev = c
	c.next = head.next

	head.next = c
	c.prev = head

	c.ts = time.Now().UnixNano()
}

func (d *Client) closeConnWithError(c *Conn, lock bool, typ string, err error) {
	// _, _, line, _ := runtime.Caller(1)
	// fmt.Println("close", c.fd, err, line)

	if lock {
		d.fdSpinLock()
	}
	d.count--
	count := int(d.count)
	c.detach()
	delete(d.fdconns, c.fd)
	if lock {
		d.fdSpinUnlock()
	}

	d.OnFdCount(count)

	if err := syscall.Close(c.fd); err != nil {
		d.OnError(resh.Error{Type: "close", Cause: err})
	}
	if err != nil {
		d.OnError(resh.Error{Type: typ, Cause: err})
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
		s.closeConnWithError(c, true, "write", err)
		return
	}

	if n == len(c.out) {
		c.out = c.out[:0]
		s.poll.ModRead(c.fd)
	} else {
		c.out = c.out[n:]
		s.poll.ModReadWrite(c.fd)
		resh.ShortWriteEmitter()
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
		d.closeConnWithError(c, true, "", nil)
		return
	}
	if err != nil {
		if err == syscall.EAGAIN {
			d.poll.ModRead(c.fd)
			return
		}
		d.closeConnWithError(c, true, "read", err)
		return
	}

	if c.callback == nil {
		d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("read: fd %d is not active", c.fd)})
		return
	}

	c.in = append(c.in, d.buffer[:n]...)
	if len(c.in) > resh.RequestMaxBytes {
		d.closeConnWithError(c, true, "oversize", fmt.Errorf("response too large: %db", len(c.in)))
		return
	}

	for len(c.in) > 0 {
		bb, err := readElement(c.in)
		if err == errWaitMore {
			break
		}
		if err != nil {
			d.closeConnWithError(c, true, "read", err)
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
		d.fdSpinLock()
		c.callback = nil
		d.attachConn(d.fdfree.head, c.detach())
		d.fdSpinUnlock()
	}
}

func (d *Client) Close() {
	if d.sslCtx != nil {
		// 	d.sslCtx.close()
	}
	d.fdSpinLock()
	for _, c := range d.fdconns {
		d.closeConnWithError(c, false, "", nil)
	}
	d.fdSpinUnlock()
	d.poll.Close()
}
