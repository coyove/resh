// Copyright 2017 Joshua J Baker. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

//go:build darwin || netbsd || freebsd || openbsd || dragonfly
// +build darwin netbsd freebsd openbsd dragonfly

package internal

import (
	"fmt"
	"syscall"
)

// Poll ...
type Poll struct {
	fd       int
	changes  []syscall.Kevent_t
	writeFds lockQueue[int]
}

// OpenPoll ...
func OpenPoll() *Poll {
	l := new(Poll)
	p, err := syscall.Kqueue()
	if err != nil {
		panic(err)
	}
	l.fd = p
	_, err = syscall.Kevent(l.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Flags:  syscall.EV_ADD | syscall.EV_CLEAR,
	}}, nil, nil)
	if err != nil {
		panic(err)
	}

	return l
}

// Close ...
func (p *Poll) Close() error {
	return syscall.Close(p.fd)
}

// Trigger ...
func (p *Poll) Trigger(fd int) error {
	p.writeFds.Add(fd)
	_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Fflags: syscall.NOTE_TRIGGER,
	}}, nil, nil)
	return err
}

// Wait ...
func (p *Poll) Wait(iter func(fd int, events uint32) error) error {
	events := make([]syscall.Kevent_t, 128)
	for {
		n, err := syscall.Kevent(p.fd, p.changes, events, nil)
		if err != nil && err != syscall.EINTR {
			return err
		}
		p.changes = p.changes[:0]
		if err := p.writeFds.SwapOutForEach(func(fd int) error {
			return iter(fd, WRITE)
		}); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			fd := int(ev.Ident)
			if fd == 0 {
				continue
			}
			switch ev.Filter {
			case syscall.EVFILT_WRITE:
				err = iter(fd, WRITE)
			case syscall.EVFILT_READ:
				if ev.Flags&syscall.EV_EOF > 0 {
					err = iter(fd, READ|EOF)
				} else {
					err = iter(fd, READ)
				}
			default:
				fmt.Println(ev)
			}
			if err != nil {
				return err
			}
		}
	}
}

// AddRead ...
func (p *Poll) AddRead(fd int) {
	p.changes = append(p.changes,
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_ADD, Filter: syscall.EVFILT_READ,
		},
	)
}

// AddReadWrite ...
func (p *Poll) AddReadWrite(fd int) {
	p.changes = append(p.changes,
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_ADD, Filter: syscall.EVFILT_WRITE,
		},
	)
}

// ModRead ...
func (p *Poll) ModRead(fd int) {
	p.AddRead(fd)
}

// ModReadWrite ...
func (p *Poll) ModReadWrite(fd int) {
	p.AddReadWrite(fd)
}
