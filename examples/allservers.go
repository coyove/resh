package main

import (
	"encoding/hex"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coyove/resh"
	"github.com/coyove/resh/examples/tools"
	"github.com/coyove/resh/redis"
	"github.com/gorilla/websocket"
)

func main() {
	ln, err := resh.Listen(false, "127.0.0.1:0")
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("listen on", ln.Addr())

	client, err := redis.NewClient("pwd", ln.Addr().String())
	if err != nil {
		panic(err)
	}
	client.OnError = func(err resh.Error) {
		log.Println("error:", err)
	}
	client.Timeout = time.Second
	client.PoolSize = 10
	// client.OnFdCount = func() {
	// 	fmt.Println(client.IdleCount())
	// }

	var succRedis, succHTTP, succWS int64
	go func() {
		for range time.Tick(time.Second) {
			log.Printf("active=%d, client=%v, redis echo=%d, http echo=%d, ws echo=%d\n",
				ln.Count(), client, succRedis, succHTTP, succWS)
		}
	}()

	ln.OnError = client.OnError
	ln.OnRedis = func(c *resh.Redis) bool {
		if c.Conn.Tag == nil {
			if c.Str(0) != "AUTH" {
				c.WriteError("No AUTH")
				return true
			}
			if c.Str(1) != "pwd" {
				c.WriteError("AUTH failed")
				return true
			}
			c.Conn.Tag = true
			c.WriteSimpleString("OK")
		} else {
			time.AfterFunc(time.Millisecond*10, func() {
				off, _ := c.Int64(1)
				c.WriteBulk(c.Get(1 + int(off) + 1))
				c.Flush()
				c.Release()
			})
		}
		return true
	}
	ln.OnHTTP = func(req *resh.HTTP) bool {
		if req.Path == "/ws" {
			time.AfterFunc(time.Millisecond*10, func() {
				req.UpgradeWebsocket(nil)
			})
		} else {
			time.AfterFunc(time.Millisecond*10, func() {
				x := []byte(req.GetQuery("a"))
				if len(x) == 0 {
					x = req.Body()
					if len(x) == 0 {
						x, _ = hex.DecodeString(req.GetHeader("x-resh"))
					}
				}
				req.Bytes(200, "text/plain", x).Flush()
				req.Release()
			})
		}
		return true
	}
	ln.OnWSData = func(ws *resh.Websocket, data []byte) {
		time.AfterFunc(time.Millisecond*10, func() {
			ws.WriteBinary(data)
		})
	}
	ln.OnWSClose = func(ws *resh.Websocket, data []byte) {
		log.Println("ws close", string(data))
	}
	go ln.Serve()

	time.Sleep(time.Second)

	for {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/ws", nil)
			if err != nil {
				log.Fatalln("ws dial:", err)
			}
			exit := make(chan bool, 1)
			m := map[string]*int{}
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
						v, ok := m[string(message)]
						if !ok {
							log.Fatalln("ws recv invalid msg")
						} else {
							*v = 1
						}
					}
				}
			}()
			for i := 0; i < 100; i++ {
				x := tools.RandData()
				m[x] = new(int)
			}
			for x := range m {
				err := c.WriteMessage(websocket.BinaryMessage, []byte(x))
				if err != nil {
					log.Fatalln("ws write:", err)
				}
			}
			time.Sleep(time.Second)
			c.WriteMessage(websocket.CloseMessage, []byte("bye"))
			c.Close()
			exit <- true

			for _, v := range m {
				if *v != 1 {
					log.Fatalln("ws miss msg")
				}
			}
			succWS += int64(len(m))
		}()
		for i := 0; i < 1000; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := 1 + rand.Intn(10)
				for i := 0; i < x; i++ {
					x := tools.RandData()[:10]

					var resp *http.Response
					var err error

					if c := rand.Intn(3); c == 0 {
						xs := url.QueryEscape(x)
						resp, err = http.Get("http://" + ln.Addr().String() + "?a=" + xs)
					} else if c == 1 {
						req, _ := http.NewRequest("GET", "http://"+ln.Addr().String(), nil)
						req.Header.Add("X-Resh", hex.EncodeToString([]byte(x)))
						resp, err = http.DefaultClient.Do(req)
					} else {
						resp, err = http.Post("http://"+ln.Addr().String(), "text/plain", strings.NewReader(x))
					}

					if err != nil {
						log.Fatalln("failed to send HTTP request:", err)
					}
					y, _ := io.ReadAll(resp.Body)
					resp.Body.Close()

					if x != string(y) {
						log.Fatalln("HTTP mismatch")
					}

					atomic.AddInt64(&succHTTP, 1)
				}
			}()
			x := 1 + rand.Intn(10)
			for i := 0; i < x; i++ {
				x := tools.RandData()
				off := rand.Intn(4) + 4
				args := []any{"test", off}
				for i := 0; i < off; i++ {
					args = append(args, tools.RandData())
				}
				args = append(args, x)

				wg.Add(1)
				client.Exec(args, func(d *redis.Reader, err error) {
					if err != nil {
						log.Fatal(err)
					}
					y := d.Str()
					if x != y {
						log.Fatalln("redis mismatch", len(x), len(y))
					}
					atomic.AddInt64(&succRedis, 1)
					wg.Done()
				})
			}
		}
		wg.Wait()
	}
}
