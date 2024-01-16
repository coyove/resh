package tools

import (
	"log"
	"math/rand"
	"sync"
	"time"
	"unsafe"

	"github.com/coyove/resh"
	"github.com/coyove/resh/redis"
)

func init() {
	resh.RequestMaxBytes = 10 * 1024 * 1024
	redis.ResponseMaxBytes = resh.RequestMaxBytes

	rand.Seed(time.Now().Unix())
	log.SetFlags(log.Lshortfile | log.Ltime)
}

func RandData() string {
	b := RandDataBytes()
	return *(*string)(unsafe.Pointer(&b))
}

func RandDataBytes() []byte {
	x := rand.Intn(65536) + 10240
	b := make([]byte, x)
	rand.Read(b)
	return b
}

func RandDataN(n int) string {
	x := rand.Intn(n/2) + n/2
	b := make([]byte, x)
	rand.Read(b)
	return *(*string)(unsafe.Pointer(&b))
}

var rmu sync.Mutex

func Intn(n int) int {
	rmu.Lock()
	defer rmu.Unlock()
	return rand.Intn(n)
}
