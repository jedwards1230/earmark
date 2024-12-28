package queue

type Queue struct {
	items chan string
}

func NewQueue() *Queue {
	return &Queue{
		items: make(chan string, 100),
	}
}

func (q *Queue) Enqueue(item string) {
	q.items <- item
}

func (q *Queue) Dequeue() (string, bool) {
	item, ok := <-q.items
	return item, ok
}

func (q *Queue) IsEmpty() bool {
	return len(q.items) == 0
}
