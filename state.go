package resh

import (
	"bytes"
	"fmt"
)

type serverReadState struct {
	// 0: read '*'        goto 1;
	//    read other byte goto 6;

	// 1: read N (number of arguments)
	// 2: read all bulk strings

	// 6: read all HTTP lines
	// 7: if Content-Length exists, read the body

	// 999: end
	stage int
	redis *Redis
	http  *HTTP
}

func (r *serverReadState) process(in []byte) error {
AGAIN:
	switch r.stage {
	case 0:
		num, w, err := readByteAndNumberCrLf('*', in)
		if err != nil {
			return err
		}
		if w == 0 {
			r.http = &HTTP{}
			r.stage = 6
		} else {
			if num > 65535 {
				return fmt.Errorf("too many redis arguments")
			}
			r.redis = &Redis{}
			r.redis.read = uint32(w)
			r.redis.nargs = uint16(num)
			r.stage = 1
		}
		goto AGAIN
	case 1:
		for len(r.redis.ai) < int(r.redis.nargs) {
			length, w, err := readByteAndNumberCrLf('$', in[r.redis.read:])
			if err != nil {
				return err
			}
			if w == 0 {
				return fmt.Errorf("invalid bulk string head %02x", in[r.redis.read])
			}
			if length > 1<<32-1 {
				return fmt.Errorf("invalid bulk string length %d", length)
			}
			x := int(r.redis.read) + w + int(length)
			if len(in) < x+2 {
				return errWaitMore
			}
			if in[x] != '\r' || in[x+1] != '\n' {
				return fmt.Errorf("invalid bulk string tail %04x", in[x:x+2])
			}
			r.redis.ai = append(r.redis.ai, [2]uint32{r.redis.read + uint32(w), uint32(length)})
			r.redis.read += uint32(w + int(length) + 2)
		}
		r.stage = 999
		r.redis.data = in[:r.redis.read]
		return nil
	case 6:
		idx := bytes.Index(in, []byte("\r\n\r\n"))
		if idx == -1 {
			return errWaitMore
		}
		r.http.data = in[:idx+4]
		if err := r.http.parse(); err != nil {
			return err
		}
		if r.http.bodyLen == 0 {
			r.stage = 8
			return nil
		}
		r.stage = 7
		fallthrough
	case 7:
		sz := int(r.http.hdrLen + r.http.bodyLen)
		if len(in) < sz {
			return errWaitMore
		}
		r.http.data = in[:sz]
		r.stage = 999
		return nil
	case 999:
		return nil
	}
	return fmt.Errorf("invalid stage %d", r.stage)
}
