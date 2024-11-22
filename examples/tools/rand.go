package tools

import (
	"encoding/base64"
	"log"
	"math/rand"
	"sync"
	"time"
	"unsafe"

	"github.com/coyove/resh"
)

func init() {
	resh.RequestMaxBytes = 10 * 1024 * 1024
	// redis.ResponseMaxBytes = resh.RequestMaxBytes

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
	base64.RawURLEncoding.Encode(b, b[:len(b)*3/4])
	copy(b[len(b)-10:], "0123456789")
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
