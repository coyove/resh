package main

import (
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/coyove/resh"
	"github.com/coyove/resh/redis"
)

func main() {
	rand.Seed(time.Now().Unix())
	log.SetFlags(log.Lshortfile | log.Ltime)

	ln, err := resh.Listen(false, "127.0.0.1:0")
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("listen on", ln.Addr())

	resh.RequestMaxBytes = 10 * 1024 * 1024
	ln.OnError = func(err resh.Error) {
		log.Println("error:", err)
	}
	ln.OnRedis = func(c *resh.Redis) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			off, _ := c.Int64(1)
			c.WriteBulk(c.Get(1 + int(off) + 1))
			c.Flush()
			c.Release()
		})
		return true
	}
	go ln.Serve()

	client, err := redis.NewClient(ln.Addr().String())
	if err != nil {
		panic(err)
	}
	client.OnError = ln.OnError

	log.Println(client.Exec([]any{"GET", "1", "zzz", "ab", "c"}, func(resp *redis.Reader) {
		fmt.Println(resp)
	}))
	select {}
}
