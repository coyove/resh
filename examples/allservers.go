package main

import (
	"context"
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
	"unsafe"

	"github.com/coyove/resh"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

func randData() string {
	x := rand.Intn(65536) + 10240
	b := make([]byte, x)
	rand.Read(b)
	return *(*string)(unsafe.Pointer(&b))
}

func main() {
	rand.Seed(time.Now().Unix())
	log.SetFlags(log.Lshortfile | log.Ltime)

	ln, err := resh.Listen(false, "127.0.0.1:0")
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("listen on", ln.Addr())

	var succRedis, succHTTP, succWS int64
	go func() {
		for range time.Tick(time.Second) {
			log.Printf("active=%d, redis echo=%d, http echo=%d, ws echo=%d\n",
				ln.Count(), succRedis, succHTTP, succWS)
		}
	}()

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
	client := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), PoolSize: 1024})

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
				x := randData()
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
						req.Header.Add("X-Resh", hex.EncodeToString([]byte(x)))
						resp, err = http.DefaultClient.Do(req)
					} else {
						resp, err = http.Post("http://"+client.Options().Addr, "text/plain", strings.NewReader(x))
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
						log.Fatalln("failed to send redis request:", err)
					}
					if x != y.(string) {
						log.Fatalln("redis mismatch")
					}

					atomic.AddInt64(&succRedis, 1)
				}
			}()
		}
		wg.Wait()
	}
}
