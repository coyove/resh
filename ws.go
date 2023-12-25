package resh

import (
	"encoding/binary"
)

type Websocket struct {
	c           *Conn
	parsedFrame wsFrame
	contFrame   []byte
	closed      bool
	closingData []byte
}

type wsFrame struct {
	data   []byte
	opcode byte
	fin    bool
	len    int
}

func (ws *Websocket) parse(in []byte) error {
	if len(in) < 2 {
		return errWaitMore
	}
	f := wsFrame{opcode: in[0] & 0xf, fin: in[0]>>7 > 0}

	var size = int(in[1] & 0x7f)
	var off int
	var mask []byte
	switch size {
	case 126:
		if len(in) < 2+2+4 {
			return errWaitMore
		}
		size = int(binary.BigEndian.Uint16(in[2:]))
		if len(in) < 2+2+4+size {
			return errWaitMore
		}
		mask = in[4:8]
		off = 8
	case 127:
		if len(in) < 2+2+6+4 {
			return errWaitMore
		}
		size = int(binary.BigEndian.Uint64(in[2:]))
		if len(in) < 2+2+6+4+size {
			return errWaitMore
		}
		mask = in[10:14]
		off = 14
	default:
		if len(in) < 2+4+size {
			return errWaitMore
		}
		mask = in[2:6]
		off = 6
	}
	f.len = off + size
	f.data = in[off : off+size]
	for i := range f.data {
		f.data[i] ^= mask[i%4]
	}

	ws.parsedFrame = f
	return nil
}

func (ws *Websocket) WriteText(msg string) {
	ws.write(1, msg)
}

func (ws *Websocket) WriteBinary(p []byte) {
	ws.write(2, btos(p))
}

func (ws *Websocket) write(typ byte, p string) {
	var tmp []byte
	tmp = append(tmp, 0x80|typ)
	if len(p) < 126 {
		tmp = append(tmp, byte(len(p)))
	} else if len(p) < 65536 {
		v := uint16(len(p))
		tmp = append(tmp, 126, byte(v>>8), byte(v))
	} else {
		tmp = binary.BigEndian.AppendUint64(append(tmp, 127), uint64(len(p)))
	}
	tmp = append(tmp, p...)
	ws.c.Write(tmp)
	ws.c.Flush()
}

func (ws *Websocket) Close() {
	ws.closed = true
	ws.write(8, "websocket: close 1000 (normal)")
}
