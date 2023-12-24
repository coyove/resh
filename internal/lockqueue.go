package internal

import (
	"sync"
	"sync/atomic"
)

type lockQueueNode[T any] struct {
	value T
	next  *lockQueueNode[T]
}

type lockQueue[T any] struct {
	pool sync.Pool
	root atomic.Pointer[lockQueueNode[T]]
}

func (q *lockQueue[T]) Add(v T) {
	if q.pool.New == nil {
		q.pool.New = func() any { return &lockQueueNode[T]{} }
	}
	root := q.root.Load()
	n := q.pool.Get().(*lockQueueNode[T])
	n.value = v
	n.next = root
	if !q.root.CompareAndSwap(root, n) {
		q.pool.Put(n)
		q.Add(v)
	}
}

func (q *lockQueue[T]) SwapOutForEach(f func(T) error) error {
	ptr := q.root.Swap(nil)
	for ptr != nil {
		tmp := *ptr
		q.pool.Put(ptr)
		if err := f(tmp.value); err != nil {
			return err
		}
		ptr = tmp.next
	}
	return nil
}
