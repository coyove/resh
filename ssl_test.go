package resh

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func randData() []byte {
	x := rand.Intn(65536) + 10240
	b := make([]byte, x)
	rand.Read(b)
	return b
}

func TestSSL(t *testing.T) {
	ln, err := Listen(false, "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	if err := ln.LoadCertFiles("testdata/cert.pem", "testdata/key.pem"); err != nil {
		panic(err)
	}
	ln.OnError = func(err error) {
		fmt.Println(err)
	}
	ln.OnHTTP = func(req *HTTP) bool {
		time.AfterFunc(time.Millisecond*10, func() {
			req.Bytes(200, "text/plain", req.Body()).Flush()
		})
		return true
	}
	ln.Timeout = time.Second
	go ln.Serve()
	time.Sleep(time.Second / 2)

	fmt.Println("serving", ln.LocalAddr())
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	for i := 0; ; i++ {
		wg := sync.WaitGroup{}
		for i := 0; i < 500; i++ {
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
		wg.Done()
		fmt.Println(time.Now(), i)
	}
}
