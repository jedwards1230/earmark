package queue

import (
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"sync"
)

type Queue struct {
	items []QueueItem
	mu    sync.Mutex
}

type QueueItem struct {
	FilePath string
	Metadata *meta.BookMetadata
}

func NewQueue() *Queue {
	return &Queue{
		items: make([]QueueItem, 0),
	}
}

func (q *Queue) Enqueue(item QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, item)
}

func (q *Queue) Dequeue() (QueueItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return QueueItem{}, false
	}

	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

func (q *Queue) IsEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}
