//go:build darwin || netbsd || freebsd || openbsd || dragonfly || linux
// +build darwin netbsd freebsd openbsd dragonfly linux

package redis

import (
	"fmt"
	"log"
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
	poll     *internal.Poll
	auth     string
	addr     syscall.Sockaddr
	buffer   []byte
	sslCtx   *resh.SSLCtx
	poolSize int

	// fdlock protects the following fields
	fdlock  atomic.Int64
	fdconns map[int]*Conn
	fdhead  *Conn
	fdtail  *Conn
	count   int

	lruhead *Conn
	lrutail *Conn

	OnFdCount func()
	OnError   func(resh.Error)
	Timeout   time.Duration
}

func NewClient(auth string, addr string, poolSize int) (*Client, error) {
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

	d.initLinklist()
	d.fdconns = make(map[int]*Conn)
	d.auth = auth
	d.poll = internal.OpenPoll()
	d.poolSize = poolSize
	d.buffer = make([]byte, 0xFFFF)
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

			if ev&syscall.EPOLLOUT > 0 {
				// if len(c.callbacks) == 0 {
				// 	d.OnError(resh.Error{Type: "warning", Cause: fmt.Errorf("write: fd %d is not active", fd)})
				// 	return nil
				// }
				d.writeConn(c)
			} else {
				d.lruMoveFront(c.lruDetach())
				d.readConn(c)
			}

			if d.Timeout > 0 {
				d.lruPurge()
			}
			return nil
		})
	}()

	return d, nil
}

type Conn struct {
	prev      *Conn
	next      *Conn
	lruprev   *Conn
	lrunext   *Conn
	lrutime   int64
	fd        int
	ssl       *resh.SSL
	in        []byte
	out       []byte
	lock      atomic.Int64
	callbacks []callback
}

type callback struct {
	index int
	f     func(*Reader, error)
}

func (s *Client) String() string {
	s.fdSpinLock()
	var counts []int
	var tot int
	for c := s.fdhead.next; c != s.fdtail; c = c.next {
		c.spinLock()
		counts = append(counts, len(c.callbacks))
		tot += len(c.callbacks)
		c.spinUnlock()
	}
	s.fdSpinUnlock()
	return fmt.Sprint(counts)
}

func (s *Client) Count() (count int) {
	s.fdSpinLock()
	for c := s.fdhead.next; c != s.fdtail; c = c.next {
		c.spinLock()
		count += len(c.callbacks)
		c.spinUnlock()
	}
	s.fdSpinUnlock()
	return
}

func (d *Client) Exec(args []any, cb func(*Reader, error)) {
	if d.OnError == nil {
		panic("missing OnError handler")
	}

	d.activateFreeConn(callback{f: cb}, func(c *Conn) {
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

func (d *Client) Debug() {
	d.fdSpinLock()
	defer d.fdSpinUnlock()

	for c := d.fdhead.next; c != d.fdtail; c = c.next {
		log.Println(c.fd, len(c.out), len(c.callbacks))
	}
}

func (d *Client) activateFreeConn(cb callback, cc func(*Conn)) {
	d.fdSpinLock()
	if d.count >= d.poolSize {
		conn := d.fdtail.prev
		conn.spinLock()
		conn.callbacks = append(conn.callbacks, cb)
		cc(conn)
		conn.spinUnlock()

		d.markBusyConn(conn.detach())
		d.fdSpinUnlock()

		d.OnFdCount()
		d.poll.Trigger(conn.fd)
		return
	}
	d.fdSpinUnlock()

	af := syscall.AF_INET
	if _, ok := d.addr.(*syscall.SockaddrInet6); ok {
		af = syscall.AF_INET6
	}
	fd, err := syscall.Socket(af, syscall.SOCK_STREAM, 0)
	if err != nil {
		cb.f(nil, err)
		return
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		cb.f(nil, err)
		return
	}
	if err := syscall.Connect(fd, d.addr); err != nil && err != syscall.EINPROGRESS {
		syscall.Close(fd)
		cb.f(nil, err)
		return
	}

	c := &Conn{}
	c.fd = fd
	if d.auth != "" {
		c.out = append(c.out, "*2\r\n$4\r\nAUTH\r\n$"...)
		c.out = strconv.AppendInt(c.out, int64(len(d.auth)), 10)
		c.out = append(c.out, "\r\n"...)
		c.out = append(c.out, d.auth...)
		c.out = append(c.out, "\r\n"...)
		c.callbacks = append(c.callbacks, callback{
			f: func(*Reader, error) {},
		})
	}
	c.callbacks = append(c.callbacks, cb)
	cc(c)

	d.fdSpinLock()
	d.fdconns[fd] = c
	d.markBusyConn(c)
	d.lruMoveFront(c)
	d.count++
	d.fdSpinUnlock()

	d.OnFdCount()
	d.poll.AddReadWrite(fd)
}

func (d *Client) closeConnWithError(c *Conn, lock bool, typ string, err error) {
	// _, _, line, _ := runtime.Caller(1)
	// fmt.Println("close", c.fd, err, line)

	if lock {
		d.fdSpinLock()
	}
	d.count--
	c.detach()
	c.lruDetach()
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
		c.spinLock()
		for _, cb := range c.callbacks {
			cb.f(nil, err)
		}
		c.spinUnlock()
	}
	if c.ssl != nil {
		c.ssl.Close()
	}
}

func (s *Client) writeConn(c *Conn) {
	c.spinLock()
	if len(c.out) == 0 {
		c.spinUnlock()
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
			c.spinUnlock()
			return
		}
		s.closeConnWithError(c, true, "write", err)
		c.spinUnlock()
		return
	}

	if n == len(c.out) {
		c.out = c.out[:0]
		s.poll.ModRead(c.fd)
	} else {
		c.out = c.out[n:]
		s.poll.ModReadWrite(c.fd)
	}
	c.spinUnlock()
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

	c.in = append(c.in, d.buffer[:n]...)
	if len(c.in) > ResponseMaxBytes {
		d.closeConnWithError(c, true, "oversize", fmt.Errorf("response too large: %db", len(c.in)))
		return
	}

	toEnd := false
	for len(c.in) > 0 {
		bb, err := readElement(c.in)
		if err == errWaitMore {
			break
		}
		if err != nil {
			d.closeConnWithError(c, true, "read", err)
			return
		}
		c.spinLock()
		if len(c.callbacks) == 0 {
			d.closeConnWithError(c, true, "read", fmt.Errorf("fd %d is not active", c.fd))
			return
		}
		r := &Reader{buf: c.in[:bb]}
		c.callbacks[0].f(r, r.err())
		c.callbacks = c.callbacks[1:]
		toEnd = len(c.callbacks) == 0
		c.spinUnlock()
		c.in = c.in[bb:]
	}

	if toEnd {
		d.fdSpinLock()
		d.markIdleConn(c)
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

func (c *Conn) spinLock() {
	for i := 0; !c.lock.CompareAndSwap(0, 1); i++ {
		runtime.Gosched()
	}
}

func (c *Conn) spinUnlock() {
	c.lock.Store(0)
}
