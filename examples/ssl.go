package main

import (
	"bytes"
	"crypto/tls"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/coyove/resh"
	"github.com/coyove/resh/examples/tools"
)

//go:embed cert.pem
var cert []byte

//go:embed key.pem
var key []byte

var role = flag.String("role", "", "")

func main() {
	flag.Parse()

	go func() {
		time.Sleep(time.Second)
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		for i := 0; ; i++ {
			wg := sync.WaitGroup{}
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					x := tools.RandDataBytes()
					resp, err := http.Post("https://127.0.0.1:6000", "text/plain", bytes.NewReader(x))
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
	}()

	switch *role {
	default:
		for i := 0; i < runtime.NumCPU(); i++ {
			ln, err := resh.Listen(true, "127.0.0.1:6000")
			if err != nil {
				panic(err)
			}
			if err := ln.LoadCertPEMs(cert, key); err != nil {
				panic(err)
			}
			ln.OnError = func(err resh.Error) {
				fmt.Println(err)
			}
			ln.OnHTTP = func(req *resh.HTTP) bool {
				time.AfterFunc(time.Millisecond*10, func() {
					req.Bytes(200, "text/plain", req.Body()).Flush()
				})
				return true
			}
			ln.Timeout = time.Second
			fmt.Println("serving", ln.Addr(), os.Getpid())

			// go func() {
			// 	for range time.Tick(time.Second) {
			// 		fmt.Println("count=", ln.Count())
			// 	}
			// }()
			go ln.Serve()
		}
		select {}
	case "gotls":
		p, _ := tls.X509KeyPair(cert, key)
		config := &tls.Config{Certificates: []tls.Certificate{p}}

		ln, err := tls.Listen("tcp", "127.0.0.1:6000", config)
		if err != nil {
			panic(err)
		}
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			w.Write(buf)
		})
		fmt.Println("serving", ln.Addr())

		http.Serve(ln, nil)
	}
}
