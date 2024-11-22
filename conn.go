package resh

import (
	"net"
	"strconv"
	"sync/atomic"
	"syscall"

	"github.com/coyove/resh/internal"
)

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
	if c.closed.Load() == 1 {
		return 0, net.ErrClosed
	}
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
	if c.closed.Load() == 1 {
		return
	}
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
