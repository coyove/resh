package resh

import (
	"strconv"
)

type Redis struct {
	Conn  *Conn
	data  []byte
	read  uint32
	nargs uint16
	ai    [][2]uint32 // [[start, length] ...]
}

func (r *Redis) Len() int {
	return len(r.ai)
}

func (r *Redis) Get(i int) []byte {
	if i < len(r.ai) {
		x := r.ai[i]
		return r.data[x[0] : x[0]+x[1]]
	}
	return nil
}

func (r *Redis) Str(i int) string {
	return btos(r.Get(i))
}

func (r *Redis) Int64(i int) (int64, error) {
	return strconv.ParseInt(r.Str(i), 10, 64)
}

func (r *Redis) Int64Default(i int, v int64) int64 {
	res, err := strconv.ParseInt(r.Str(i), 10, 64)
	if err != nil {
		res = v
	}
	return res
}

func (r *Redis) WriteRawBytes(p []byte) *Redis {
	r.Conn.Write(p)
	return r
}

func (r *Redis) WriteError(err string) *Redis {
	r.Conn.spinLock()
	r.Conn.out = append(r.Conn.out, '-')
	r.Conn.out = append(r.Conn.out, err...)
	r.Conn.out = append(r.Conn.out, "\r\n"...)
	r.Conn.spinUnlock()
	return r
}

func (r *Redis) WriteSimpleString(p string) *Redis {
	r.Conn.spinLock()
	r.Conn.out = append(r.Conn.out, '+')
	r.Conn.out = append(r.Conn.out, p...)
	r.Conn.out = append(r.Conn.out, "\r\n"...)
	r.Conn.spinUnlock()
	return r
}

func (r *Redis) WriteBulk(p []byte) *Redis {
	return r.WriteBulkString(btos(p))
}

func (r *Redis) WriteBulkString(p string) *Redis {
	r.Conn.spinLock()
	r.Conn.out = append(r.Conn.out, '$')
	r.Conn.out = strconv.AppendInt(r.Conn.out, int64(len(p)), 10)
	r.Conn.out = append(r.Conn.out, "\r\n"...)
	r.Conn.out = append(r.Conn.out, p...)
	r.Conn.out = append(r.Conn.out, "\r\n"...)
	r.Conn.spinUnlock()
	return r
}

func (r *Redis) WriteArrayBegin(n int) *Redis {
	r.Conn.spinLock()
	r.Conn.out = append(r.Conn.out, '*')
	r.Conn.out = strconv.AppendInt(r.Conn.out, int64(n), 10)
	r.Conn.out = append(r.Conn.out, "\r\n"...)
	r.Conn.spinUnlock()
	return r
}

func (r *Redis) Flush() *Redis {
	r.Conn.Flush()
	return r
}

func (r *Redis) Release() {
	r.Conn.ReuseInputBuffer(r.data)
}
