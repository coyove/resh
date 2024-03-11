//go:build darwin || netbsd || freebsd || openbsd || dragonfly || linux
// +build darwin netbsd freebsd openbsd dragonfly linux

package redis

import (
	"fmt"
	"net"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
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

	// fdlock protects the following fields
	fdlock  sync.Mutex
	fdconns map[int]*Conn
	fdIdle  struct {
		head, tail *Conn
		count      int
	}
	fdActive struct {
		head, tail *Conn
		count      int
	}

	OnFdCount func()
	OnError   func(resh.Error)
	Timeout   time.Duration
	PoolSize  int
}

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

	d.fdIdle.head = &Conn{}
	d.fdIdle.tail = &Conn{}
	d.fdIdle.head.next = d.fdIdle.tail
	d.fdIdle.tail.prev = d.fdIdle.head
	d.fdActive.head = &Conn{}
	d.fdActive.tail = &Conn{}
	d.fdActive.head.next = d.fdActive.tail
	d.fdActive.tail.prev = d.fdActive.head

	d.auth = auth
	d.poll = internal.OpenPoll()
	d.buffer = make([]byte, 0xFFFF)
	d.fdconns = make(map[int]*Conn)
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
			d.fdSpinLock()
			c, ok := d.fdconns[fd]
			d.fdSpinUnlock()
			if !ok {
				d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("fd %d not found", fd)})
				return nil
			}

			d.fdSpinLock()
			if c.callback != nil {
				d.attachConn(d.fdActive.head, c.detach())
			}
			if d.Timeout > 0 {
				for conn, now := d.fdActive.tail.prev, time.Now().UnixNano(); conn != nil && conn != d.fdActive.head; conn = conn.prev {
					if conn.ts < now-int64(d.Timeout) {
						d.closeConnWithError(conn, false, "timeout", fmt.Errorf("connection timed out (fd=%d)", conn.fd))
					} else {
						break
					}
				}
			}
			d.fdSpinUnlock()

			if ev&syscall.EPOLLOUT > 0 {
				if c.callback == nil {
					d.closeConnWithError(c, true, "write", fmt.Errorf("write: fd %d is not active", fd))
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
	lock     atomic.Int64
	prev     *Conn
	next     *Conn
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

func (c *Conn) detach() *Conn {
	c.prev.next = c.next
	c.next.prev = c.prev
	return c
}

func (d *Client) String() string {
	d.fdSpinLock()
	defer d.fdSpinUnlock()
	idle := 0
	for c := d.fdIdle.head.next; c != d.fdIdle.tail; c = c.next {
		idle++
	}
	active := 0
	for c := d.fdActive.head.next; c != d.fdActive.tail; c = c.next {
		active++
	}
	return fmt.Sprintf("(host=%v, idle=%d, active=%d, total=%d)",
		internal.SockaddrToAddr(d.addr), idle, active, len(d.fdconns))
}

func (s *Client) ActiveCount() int { return s.fdActive.count }

func (s *Client) IdleCount() int { return s.fdIdle.count }

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

func (d *Client) fdSpinLock() {
	d.fdlock.Lock()
}

func (d *Client) fdSpinUnlock() {
	d.fdlock.Unlock()
}

func (d *Client) activateFreeConn(cb func(*Reader, error), cc func(*Conn)) {
RETRY:
	d.fdSpinLock()
	if conn := d.fdIdle.head.next; conn != nil && conn != d.fdIdle.tail {
		if len(conn.out) > 0 {
			panic("BUG")
		}

		conn.callback = cb
		conn.out = conn.out[:0]
		cc(conn)
		d.attachConn(d.fdActive.head, conn.detach())
		d.fdIdle.count--
		d.fdActive.count++

		// Free idle connections
		for d.PoolSize > 0 && len(d.fdconns) > d.PoolSize {
			conn := d.fdIdle.tail.prev
			if conn != nil && conn != d.fdIdle.head {
				d.closeConnWithError(conn, false, "", nil)
			} else {
				break
			}
		}
		d.fdSpinUnlock()

		d.OnFdCount()
		d.poll.Trigger(conn.fd)
		return
	}
	d.fdSpinUnlock()

	// No lock here, so we only have a rough number of total connections.
	if d.PoolSize > 0 && d.fdActive.count+d.fdIdle.count >= d.PoolSize {
		goto RETRY
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

	d.fdSpinLock()
	d.fdconns[fd] = c
	d.attachConn(d.fdActive.head, c)
	d.fdActive.count++
	d.fdSpinUnlock()

	d.OnFdCount()
	d.poll.AddReadWrite(fd)
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
	if c.callback != nil {
		d.fdActive.count--
	} else {
		d.fdIdle.count--
	}
	c.detach()
	delete(d.fdconns, c.fd)
	if lock {
		d.fdSpinUnlock()
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
		s.closeConnWithError(c, true, "write", err)
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
	if len(c.in) > ResponseMaxBytes {
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
		if d.PoolSize == 0 || len(d.fdconns) <= d.PoolSize {
			c.callback = nil
			d.attachConn(d.fdIdle.head, c.detach())
			d.fdActive.count--
			d.fdIdle.count++
		} else {
			d.closeConnWithError(c, false, "free", nil)
		}
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
