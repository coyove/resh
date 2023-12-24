package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime/pprof"
	"time"

	"github.com/coyove/resh"
	reuseport "github.com/kavu/go_reuseport"
)

func main() {
	rand.Seed(time.Now().Unix())
	log.SetFlags(log.Lshortfile | log.Ltime)
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
		ln, _ := resh.Listen(true, ":7777")
		ln.OnError = func(err error) {
			log.Println(err)
		}
		ln.OnHTTP = func(req *resh.HTTP) bool {
			req.Text(200, "hello world").Release()
			return true
		}
		ln.Serve()
	}
}
