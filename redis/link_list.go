package redis

import (
	"fmt"
	"time"
)

func (d *Client) initLinklist() {
	d.fdhead = &Conn{}
	d.fdtail = &Conn{}
	d.fdhead.next = d.fdtail
	d.fdtail.prev = d.fdhead

	d.lruhead = &Conn{}
	d.lrutail = &Conn{}
	d.lruhead.lrunext = d.lrutail
	d.lrutail.lruprev = d.lruhead
}

func (d *Client) markBusyConn(c *Conn) {
	d.fdhead.next.prev = c
	c.next = d.fdhead.next

	d.fdhead.next = c
	c.prev = d.fdhead
}

func (d *Client) markIdleConn(c *Conn) {
	c.detach()
	d.fdtail.prev.next = c
	c.prev = d.fdtail.prev
	d.fdtail.prev = c
	c.next = d.fdtail
}

func (d *Client) lruMoveFront(c *Conn) {
	d.lruhead.lrunext.lruprev = c
	c.lrunext = d.lruhead.lrunext

	d.lruhead.lrunext = c
	c.lruprev = d.lruhead

	c.lrutime = time.Now().UnixNano()
}

func (d *Client) lruPurge() {
	for conn, now := d.lrutail.lruprev, time.Now().UnixNano(); conn != nil && conn != d.lruhead; conn = conn.lruprev {
		if conn.lrutime < now-int64(d.Timeout) {
			if len(conn.callbacks) == 0 {
				d.closeConnWithError(conn, true, "", nil)
			} else {
				d.closeConnWithError(conn, true, "timeout", fmt.Errorf("connection timed out (fd=%d)", conn.fd))
			}
		} else {
			break
		}
	}
}

func (c *Conn) lruDetach() *Conn {
	c.lruprev.lrunext = c.lrunext
	c.lrunext.lruprev = c.lruprev
	return c
}

func (c *Conn) detach() *Conn {
	c.prev.next = c.next
	c.next.prev = c.prev
	return c
}
