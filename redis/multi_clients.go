package redis

import (
	"sync/atomic"

	"github.com/coyove/resh"
)

type MultiCoreClients struct {
	clients []*Client
	counts  [][2]int
	ctr     atomic.Int64
}

func NewMultiCoreClients(n, poolSize int, auth, addr string, onError func(resh.Error)) (*MultiCoreClients, error) {
	clients := make([]*Client, n)
	counts := make([][2]int, n)

	c := &MultiCoreClients{
		clients: clients,
		counts:  counts,
	}

	var err error
	for i := range clients {
		i := i
		clients[i], err = NewClient(poolSize/n+1, auth, addr)
		if err != nil {
			return nil, err
		}
		clients[i].OnFdCount = func() {
			counts[i] = [2]int{
				clients[i].IdleCount(),
				clients[i].ActiveCount(),
			}
		}
		clients[i].OnError = onError
	}

	return c, nil
}

func (c *MultiCoreClients) Close() {
	for _, a := range c.clients {
		a.Close()
	}
}

func (c *MultiCoreClients) Exec(args []any, f func(*Reader, error)) {
	i := c.ctr.Add(1) % int64(len(c.clients))
	cc := c.clients[i]
	cc.Exec(args, f)
}

func (c *MultiCoreClients) IdleCount() (tot int) {
	for _, c := range c.counts {
		tot += c[0]
	}
	return
}

func (c *MultiCoreClients) ActiveCount() (tot int) {
	for _, c := range c.counts {
		tot += c[1]
	}
	return
}
