package redis

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"unsafe"
)

var errWaitMore = fmt.Errorf("wait more")
var crlf = []byte("\r\n")

func readElement(in []byte) (int, error) {
	if len(in) == 0 {
		return 0, errWaitMore
	}
	switch in[0] {
	case '$':
		if idx := bytes.Index(in, crlf); idx > 0 {
			sz, err := strconv.Atoi(btos(in[1:idx]))
			if err != nil {
				return 0, err
			}
			if len(in) >= idx+2+int(sz)+2 {
				buf := in[:idx+2+int(sz)+2]
				if buf[len(buf)-2] == '\r' && buf[len(buf)-1] == '\n' {
					return len(buf), nil
				}
				return 0, fmt.Errorf("invalid end of bulk string")
			}
		}
	case '+', '-', ':':
		if idx := bytes.Index(in, crlf); idx > 0 {
			return idx + 2, nil
		}
	case '*':
		if idx := bytes.Index(in, crlf); idx > 0 {
			count, err := strconv.Atoi(btos(in[1:idx]))
			if err != nil {
				return 0, err
			}
			c := idx + 2
			for i := 0; i < count; i++ {
				x, err := readElement(in[c:])
				if err != nil {
					return 0, err
				}
				c += x
			}
			return c, nil
		}
	default:
		return 0, fmt.Errorf("invalid RESP first byte %x", in[0])
	}
	return 0, errWaitMore
}

func btos(in []byte) string {
	return unsafe.String(unsafe.SliceData(in), len(in))
}

type Reader struct {
	buf []byte
}

func (r *Reader) Next() any {
	typ, i, s, d := r.readNext()
	switch typ {
	case 's':
		return s
	case 'i':
		return i
	case 'e':
		return errors.New(btos(s))
	case 'n':
		return nil
	}
	return d
}

func (r *Reader) err() error {
	o := r.buf
	typ, _, s, _ := r.readNext()
	if typ == 'e' {
		return errors.New(btos(s))
	}
	r.buf = o
	return nil
}

func (r *Reader) Bytes() []byte {
	_, _, s, _ := r.readNext()
	return s
}

func (r *Reader) Str() string {
	_, _, s, _ := r.readNext()
	return btos(s)
}

func (r *Reader) Int64() int64 {
	_, i, _, _ := r.readNext()
	return i
}

func (r *Reader) Array() *Reader {
	_, _, _, a := r.readNext()
	return a
}

func (r *Reader) readNext() (byte, int64, []byte, *Reader) {
	if len(r.buf) == 0 {
		return 'n', 0, nil, nil
	}
	switch head := r.buf[0]; head {
	case '$':
		idx := bytes.Index(r.buf, crlf)
		sz, _ := strconv.Atoi(btos(r.buf[1:idx]))
		data := r.buf[idx+2 : idx+2+sz]
		r.buf = r.buf[idx+2+sz+2:]
		return 's', 0, data, nil
	case '+', '-', ':':
		idx := bytes.Index(r.buf, crlf)
		data := r.buf[1:idx]
		r.buf = r.buf[idx+2:]
		switch head {
		case '+':
			return 's', 0, data, nil
		case '-':
			return 'e', 0, data, nil
		case ':':
			v, _ := strconv.ParseInt(btos(data), 10, 64)
			return 'i', v, nil, nil
		}
	case '*':
		idx := bytes.Index(r.buf, crlf)
		count, _ := strconv.Atoi(btos(r.buf[1:idx]))
		c := idx + 2
		for i := 0; i < count; i++ {
			x, _ := readElement(r.buf[c:])
			c += x
		}
		d := &Reader{buf: r.buf[:c]}
		r.buf = r.buf[c:]
		return 'a', 0, nil, d
	}
	panic("BUG")
}

func (r *Reader) String() string {
	d := *r
	buf := bytes.NewBufferString("(")
	for a, i := d.Next(), 0; a != nil; a, i = d.Next(), i+1 {
		if i > 0 {
			buf.WriteString(",")
		}
		fmt.Fprint(buf, a)
	}
	buf.WriteString(")")
	return buf.String()
}
