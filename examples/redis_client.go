package main

import (
	"flag"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coyove/resh"
	"github.com/coyove/resh/examples/tools"
	"github.com/coyove/resh/redis"
)

var addr = flag.String("addr", "", "")
var auth = flag.String("auth", "", "")
var clients = flag.Int("clients", 200, "")
var qps = flag.Int("qps", 100, "")
var poolSize = flag.Int("pool", 1000, "")
var dataSize = flag.Int("data", 10000, "")

func main() {
	flag.Parse()

	client, err := redis.NewClient(*poolSize, *auth, *addr)
	if err != nil {
		panic(err)
	}
	client.OnError = func(err resh.Error) {
		log.Println(err)
	}
	client.Timeout = time.Second * 5

	var workers atomic.Int64
	go func() {
		for range time.Tick(time.Second) {
			log.Printf("active=%d, idle=%d, workers=%d", client.ActiveCount(), client.IdleCount(), workers.Load())
		}
	}()

	m := map[string]string{}
	for i := 0; ; i++ {
		start := time.Now()
		var wg sync.WaitGroup
		ki := 0
		tot := 0.0
		for c := 0; c < *clients; c++ {
			for q := 0; q < *qps; q++ {
				wg.Add(1)
				workers.Add(1)

				var cmd []any
				var get, met bool
				var k string

				if i%10 == 0 {
					k = fmt.Sprintf("resh-test-%d", ki)
					ki++
					v := tools.RandDataN(*dataSize)
					cmd = []any{"SET", k, v}
					m[k] = v
				} else {
					k = fmt.Sprintf("resh-test-%d", tools.Intn(*clients**qps))
					cmd = []any{"GET", k}
					get = true
				}

				start := time.Now()
				client.Exec(cmd, func(resp *redis.Reader, err error) {
					if err != nil {
						_, fn, line, _ := runtime.Caller(1)
						log.Fatalln(fn, line, err)
					}
					if met {
						log.Fatal("double execution")
					}
					met = true
					if get {
						v2 := resp.Str()
						old := m[k]
						if v2 != old {
							log.Fatalf("value mismatch %s %d %v", k, len(v2), len(old))
						}
					} else {
						if r := resp.Str(); r != "OK" {
							log.Fatalf("set %q", r)
						}
					}
					workers.Add(-1)
					wg.Done()
					tot += time.Since(start).Seconds()
				})
			}
		}
		wg.Wait()
		fmt.Println("round", i, "in", time.Since(start), tot/float64(*clients)/float64(*qps))
	}
}
