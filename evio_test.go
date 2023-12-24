// Copyright 2017 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package resh

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	reuseport "github.com/kavu/go_reuseport"
)

func TestServer(t *testing.T) {
	out, _ := os.Create("cpuprofile")
	pprof.StartCPUProfile(out)
	time.AfterFunc(time.Second*5, func() { pprof.StopCPUProfile() })

	ln, _ := Listen(false, "127.0.0.1:6000")

	var succ, succ2 int64

	go func() {
		for range time.Tick(time.Second) {
			fmt.Println("count", ln.Count(), time.Now(), succ, "http=", succ2)
		}
	}()

	RequestMaxBytes = 10 * 1024 * 1024
	ln.OnRedis = func(c *Redis) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			off, _ := c.Int64(1)
			c.WriteBulk(c.Get(1 + int(off) + 1))
			c.Flush()
			c.Release()
		})
		return true
	}
	ln.OnError = func(err error) {
		fmt.Println(err)
	}
	ln.OnHTTP = func(req *HTTP) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			x := []byte(req.GetQuery("a"))
			if len(x) == 0 {
				x = req.Body()
				if len(x) == 0 {
					x, _ = hex.DecodeString(req.GetHeader("x-zzz"))
				}
			}
			req.Bytes(200, "text/plain", x).Flush()
			req.Release()
		})
		return true
	}
	go ln.Serve()

	rand.Seed(time.Now().Unix())
	randData := func() string {
		x := rand.Intn(65536) + 10240
		if rand.Intn(10) == 0 {
			x *= 10
		}
		b := make([]byte, x)
		rand.Read(b)
		return string(b)
	}

	time.Sleep(time.Second)
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6000", PoolSize: 1024})

	for i := 0; i < 100e6; i++ {
		var wg sync.WaitGroup
		for i := 0; i < 1000; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				x := 1 + rand.Intn(10)
				for i := 0; i < x; i++ {
					x := randData()[:10]

					var resp *http.Response
					var err error

					if c := rand.Intn(3); c == 0 {
						xs := url.QueryEscape(x)
						resp, err = http.Get("http://" + client.Options().Addr + "?a=" + xs)
					} else if c == 1 {
						req, _ := http.NewRequest("GET", "http://"+client.Options().Addr, nil)
						req.Header.Add("X-ZZZ", hex.EncodeToString([]byte(x)))
						resp, err = http.DefaultClient.Do(req)
					} else {
						resp, err = http.Post("http://"+client.Options().Addr, "text/plain", strings.NewReader(x))
					}

					if err != nil {
						fmt.Println("wrong", err)
						os.Exit(1)
					}
					y, _ := io.ReadAll(resp.Body)
					resp.Body.Close()

					if x != btos(y) {
						fmt.Println("wrong", i)
						os.Exit(1)
					}

					atomic.AddInt64(&succ2, 1)
				}
			}()
			go func() {
				defer wg.Done()
				x := 1 + rand.Intn(10)
				for i := 0; i < x; i++ {
					x := randData()
					off := rand.Intn(4) + 4
					args := []any{"test", off}
					for i := 0; i < off; i++ {
						args = append(args, randData())
					}
					args = append(args, x)

					y, err := client.Do(context.TODO(), args...).Result()
					if err != nil {
						fmt.Println("wrong", err)
						os.Exit(1)
					}
					if x != y.(string) {
						fmt.Println("wrong", i)
						os.Exit(1)
					}

					atomic.AddInt64(&succ, 1)
				}
			}()
		}
		wg.Wait()
	}
	fmt.Println("ok")
	select {}
}

func TestWebsocket(t *testing.T) {
	ln, _ := Listen(false, "127.0.0.1:6000")

	var succ, succ2 int64

	go func() {
		for range time.Tick(time.Second) {
			fmt.Println("count", ln.Count(), time.Now(), succ, "http=", succ2)
		}
	}()

	RequestMaxBytes = 10 * 1024 * 1024
	ln.OnError = func(err error) {
		fmt.Println(err)
	}

	ln.OnHTTP = func(req *HTTP) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			ws := req.UpgradeWebsocket(nil)
			ws.OnData = func(ws *Websocket, data []byte) {
				time.AfterFunc(time.Millisecond*10, func() {
					ws.WriteBinary(data)
				})
			}
			ws.OnClose = func(ws *Websocket, data []byte) {
				fmt.Println("server close", strconv.Quote(string(data)))
			}
		})
		return true
	}
	go ln.Serve()

	rand.Seed(time.Now().Unix())
	randData := func() string {
		// return strings.Repeat("1", 1000)
		x := rand.Intn(60000) + 2560
		b := make([]byte, x)
		for i := range b {
			b[i] = 'a' + byte(rand.Int()%26)
		}
		return string(b)
	}

	time.Sleep(time.Second)

	wg := sync.WaitGroup{}
	for c := 0; c < 100; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:6000", nil)
			if err != nil {
				panic(err)
			}
			exit := make(chan bool, 1)
			m := map[string]*int{}
			miss := atomic.Int64{}
			go func() {
				for {
					select {
					case <-exit:
						return
					default:
						_, message, err := c.ReadMessage()
						if err != nil {
							return
						}
						// fmt.Println("recv", string(message))
						v, ok := m[btos(message)]
						if !ok {
							miss.Add(1)
						} else {
							*v = 1
						}
					}
				}
			}()
			for i := 0; i < 100; i++ {
				x := randData()
				m[x] = new(int)
			}
			i := 0
			for x := range m {
				// fmt.Println(i)
				err := c.WriteMessage(websocket.BinaryMessage, []byte(x))
				if err != nil {
					panic(err)
				}
				i++
			}
			time.Sleep(time.Second)
			c.Close()
			exit <- true

			for _, v := range m {
				if *v != 1 {
					miss.Add(1)
				}
			}
			fmt.Println("miss=", miss.Load())
		}()
	}
	wg.Wait()
}

func TestBenchHTTP(t *testing.T) {
	if os.Getenv("GO") != "" {
		fmt.Println("native go")
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("hello world"))
		})
		ln, _ := reuseport.Listen("tcp", ":7777")
		http.Serve(ln, nil)
	} else {
		if os.Getenv("DEBUG") == "1" {
			out, _ := os.Create("cpuprofile")
			pprof.StartCPUProfile(out)

			time.AfterFunc(time.Second*30, func() {
				pprof.StopCPUProfile()
				out.Close()
			})
		}

		fmt.Println("resh", os.Getpid())
		ln, _ := Listen(true, ":7777")
		ln.OnError = func(err error) {
			t.Log(err)
		}
		ln.OnHTTP = func(req *HTTP) bool {
			req.Text(200, "hello world").Release()
			return true
		}
		ln.Serve()
	}
}
