package main

import (
	"bytes"
	"crypto/tls"
	_ "embed"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coyove/resh"
)

//go:embed cert.pem
var cert []byte

//go:embed key.pem
var key []byte

func randData() []byte {
	x := rand.Intn(65536) + 10240
	b := make([]byte, x)
	rand.Read(b)
	return b
}

func main() {
	ln, err := resh.Listen(false, "127.0.0.1:6000")
	if err != nil {
		panic(err)
	}
	if err := ln.LoadCertPEMs(cert, key); err != nil {
		panic(err)
	}
	ln.OnError = func(err error) {
		fmt.Println(err)
	}
	ln.OnHTTP = func(req *resh.HTTP) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			req.Bytes(200, "text/plain", req.Body()).Flush()
		})
		return true
	}
	ln.Timeout = time.Second
	fmt.Println("serving", ln.LocalAddr())

	go func() {
		for range time.Tick(time.Second) {
			fmt.Println(ln.Count())
		}
	}()
	go ln.Serve()

	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	for i := 0; ; i++ {
		wg := sync.WaitGroup{}
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := randData()
				resp, err := http.Post("https://"+ln.LocalAddr().String(), "text/plain", bytes.NewReader(x))
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				defer resp.Body.Close()
				buf, _ := io.ReadAll(resp.Body)
				if !bytes.Equal(buf, x) {
					fmt.Println("echo mismatch")
					os.Exit(1)
				}
			}()
		}
		wg.Wait()
		fmt.Println(time.Now(), i)
	}
	fmt.Println("waiting")
	select {}
}
