package redis

import (
	"sync"
	"sync/atomic"
)

type fdMap[T any] struct {
	mu    sync.Mutex
	fds   atomic.Pointer[[]atomic.Pointer[T]]
	count atomic.Int64
}

func (m *fdMap[T]) Add(fd int, v *T) {
	if fd >= len(*m.fds.Load()) {
		m.mu.Lock()
		fds := *m.fds.Load()
		for len(fds) <= fd {
			fds = append(fds, atomic.Pointer[T]{})
		}
		m.fds.Store(&fds)
		m.mu.Unlock()
	}

	old := (*m.fds.Load())[fd].Swap(v)
	if old != nil {
		panic("BUG")
	}
	m.count.Add(1)
}

func (m *fdMap[T]) Get(fd int) (v *T) {
	fds := *m.fds.Load()
	if fd >= len(fds) {
		return nil
	}
	return fds[fd].Load()
}

func (m *fdMap[T]) Delete(fd int) {
	m.mu.Lock()
	fds := *m.fds.Load()
	if fd < len(fds) {
		old := fds[fd].Swap(nil)
		if old != nil {
			m.count.Add(-1)
		}
	}
	m.mu.Unlock()
}

func (m *fdMap[T]) Foreach(f func(int, *T)) {
	fds := *m.fds.Load()
	for fd := range fds {
		if v := fds[fd].Load(); v != nil {
			f(fd, v)
		}
	}
}

func (m *fdMap[T]) Len() int {
	return int(m.count.Load())
}

func (m *fdMap[T]) Cap() int {
	return len(*m.fds.Load())
}
