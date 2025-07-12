package queue

import (
	"fmt"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"sync"
	"testing"
	"time"
)

func TestNewQueue(t *testing.T) {
	q := NewQueue()

	// NewQueue() always returns a valid queue

	if !q.IsEmpty() {
		t.Error("Expected new queue to be empty")
	}

	if q.items == nil {
		t.Error("Expected items slice to be initialized")
	}
}

func TestEnqueueDequeue(t *testing.T) {
	q := NewQueue()

	// Test with a simple item
	item1 := QueueItem{
		FilePath: "/test/file1.mp3",
		Metadata: &meta.BookMetadata{
			Title:  "Test Book 1",
			Author: "Test Author 1",
		},
	}

	// Enqueue item
	q.Enqueue(item1)

	if q.IsEmpty() {
		t.Error("Expected queue to not be empty after enqueue")
	}

	// Dequeue item
	dequeuedItem, ok := q.Dequeue()
	if !ok {
		t.Error("Expected dequeue to succeed")
	}

	if dequeuedItem.FilePath != item1.FilePath {
		t.Errorf("Expected dequeued FilePath %s, got %s", item1.FilePath, dequeuedItem.FilePath)
	}

	if dequeuedItem.Metadata.Title != item1.Metadata.Title {
		t.Errorf("Expected dequeued Title %s, got %s", item1.Metadata.Title, dequeuedItem.Metadata.Title)
	}

	if dequeuedItem.Metadata.Author != item1.Metadata.Author {
		t.Errorf("Expected dequeued Author %s, got %s", item1.Metadata.Author, dequeuedItem.Metadata.Author)
	}

	if !q.IsEmpty() {
		t.Error("Expected queue to be empty after dequeue")
	}
}

func TestDequeueEmptyQueue(t *testing.T) {
	q := NewQueue()

	// Try to dequeue from empty queue
	item, ok := q.Dequeue()
	if ok {
		t.Error("Expected dequeue from empty queue to return false")
	}

	if item.FilePath != "" {
		t.Error("Expected empty item from empty queue")
	}

	if item.Metadata != nil {
		t.Error("Expected nil metadata from empty queue")
	}
}

func TestFIFOOrder(t *testing.T) {
	q := NewQueue()

	// Create multiple items
	items := []QueueItem{
		{
			FilePath: "/test/file1.mp3",
			Metadata: &meta.BookMetadata{Title: "Book 1", Author: "Author 1"},
		},
		{
			FilePath: "/test/file2.mp3",
			Metadata: &meta.BookMetadata{Title: "Book 2", Author: "Author 2"},
		},
		{
			FilePath: "/test/file3.mp3",
			Metadata: &meta.BookMetadata{Title: "Book 3", Author: "Author 3"},
		},
	}

	// Enqueue all items
	for _, item := range items {
		q.Enqueue(item)
	}

	// Dequeue and verify FIFO order
	for i, expectedItem := range items {
		dequeuedItem, ok := q.Dequeue()
		if !ok {
			t.Fatalf("Expected dequeue to succeed for item %d", i)
		}

		if dequeuedItem.FilePath != expectedItem.FilePath {
			t.Errorf("Expected item %d FilePath %s, got %s", i, expectedItem.FilePath, dequeuedItem.FilePath)
		}

		if dequeuedItem.Metadata.Title != expectedItem.Metadata.Title {
			t.Errorf("Expected item %d Title %s, got %s", i, expectedItem.Metadata.Title, dequeuedItem.Metadata.Title)
		}
	}

	if !q.IsEmpty() {
		t.Error("Expected queue to be empty after dequeuing all items")
	}
}

func TestIsEmpty(t *testing.T) {
	q := NewQueue()

	// Test empty queue
	if !q.IsEmpty() {
		t.Error("Expected new queue to be empty")
	}

	// Add item
	item := QueueItem{
		FilePath: "/test/file.mp3",
		Metadata: &meta.BookMetadata{Title: "Test", Author: "Test"},
	}
	q.Enqueue(item)

	// Test non-empty queue
	if q.IsEmpty() {
		t.Error("Expected queue with item to not be empty")
	}

	// Remove item
	q.Dequeue()

	// Test empty again
	if !q.IsEmpty() {
		t.Error("Expected queue to be empty after removing last item")
	}
}

func TestMultipleEnqueueDequeue(t *testing.T) {
	q := NewQueue()

	// Test multiple enqueue/dequeue cycles
	for i := 0; i < 10; i++ {
		item := QueueItem{
			FilePath: fmt.Sprintf("/test/file%d.mp3", i),
			Metadata: &meta.BookMetadata{
				Title:  fmt.Sprintf("Book %d", i),
				Author: fmt.Sprintf("Author %d", i),
			},
		}

		q.Enqueue(item)

		dequeuedItem, ok := q.Dequeue()
		if !ok {
			t.Fatalf("Expected dequeue to succeed for iteration %d", i)
		}

		if dequeuedItem.FilePath != item.FilePath {
			t.Errorf("Expected FilePath %s, got %s in iteration %d", item.FilePath, dequeuedItem.FilePath, i)
		}

		if !q.IsEmpty() {
			t.Errorf("Expected queue to be empty after iteration %d", i)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	q := NewQueue()
	const numWorkers = 10
	const itemsPerWorker = 100

	var wg sync.WaitGroup

	// Start producer goroutines
	for worker := 0; worker < numWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < itemsPerWorker; i++ {
				item := QueueItem{
					FilePath: fmt.Sprintf("/test/worker%d_file%d.mp3", workerID, i),
					Metadata: &meta.BookMetadata{
						Title:  fmt.Sprintf("Worker %d Book %d", workerID, i),
						Author: fmt.Sprintf("Worker %d Author %d", workerID, i),
					},
				}
				q.Enqueue(item)

				// Small delay to increase chance of race conditions
				time.Sleep(time.Microsecond)
			}
		}(worker)
	}

	// Start consumer goroutines
	dequeuedItems := make(chan QueueItem, numWorkers*itemsPerWorker)
	for worker := 0; worker < numWorkers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < itemsPerWorker; i++ {
				for {
					item, ok := q.Dequeue()
					if ok {
						dequeuedItems <- item
						break
					}
					// Small delay before retrying
					time.Sleep(time.Microsecond)
				}
			}
		}()
	}

	// Wait for all workers to complete
	wg.Wait()
	close(dequeuedItems)

	// Verify we got all items
	itemCount := 0
	for range dequeuedItems {
		itemCount++
	}

	expectedItems := numWorkers * itemsPerWorker
	if itemCount != expectedItems {
		t.Errorf("Expected %d items, got %d", expectedItems, itemCount)
	}

	if !q.IsEmpty() {
		t.Error("Expected queue to be empty after all items processed")
	}
}

func TestQueueItemStructure(t *testing.T) {
	// Test QueueItem with nil metadata
	item1 := QueueItem{
		FilePath: "/test/file.mp3",
		Metadata: nil,
	}

	if item1.FilePath != "/test/file.mp3" {
		t.Errorf("Expected FilePath /test/file.mp3, got %s", item1.FilePath)
	}

	if item1.Metadata != nil {
		t.Error("Expected nil metadata")
	}

	// Test QueueItem with complete metadata
	item2 := QueueItem{
		FilePath: "/test/file2.mp3",
		Metadata: &meta.BookMetadata{
			ID:     1,
			Title:  "Complete Book",
			Author: "Complete Author",
			ISBN:   "1234567890",
			ASIN:   "B1234567890",
		},
	}

	if item2.Metadata.Title != "Complete Book" {
		t.Error("Expected metadata to be preserved")
	}
}

func TestQueueCapacity(t *testing.T) {
	q := NewQueue()

	// Test adding many items (stress test)
	const largeNumber = 10000

	// Enqueue many items
	for i := 0; i < largeNumber; i++ {
		item := QueueItem{
			FilePath: fmt.Sprintf("/test/file%d.mp3", i),
			Metadata: &meta.BookMetadata{
				Title:  fmt.Sprintf("Book %d", i),
				Author: "Test Author",
			},
		}
		q.Enqueue(item)
	}

	// Verify all items can be dequeued
	for i := 0; i < largeNumber; i++ {
		item, ok := q.Dequeue()
		if !ok {
			t.Fatalf("Failed to dequeue item %d", i)
		}

		expectedPath := fmt.Sprintf("/test/file%d.mp3", i)
		if item.FilePath != expectedPath {
			t.Errorf("Expected FilePath %s, got %s at position %d", expectedPath, item.FilePath, i)
		}
	}

	if !q.IsEmpty() {
		t.Error("Expected queue to be empty after dequeuing all items")
	}
}

// Benchmark tests
func BenchmarkEnqueue(b *testing.B) {
	q := NewQueue()
	item := QueueItem{
		FilePath: "/test/benchmark.mp3",
		Metadata: &meta.BookMetadata{
			Title:  "Benchmark Book",
			Author: "Benchmark Author",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Enqueue(item)
	}
}

func BenchmarkDequeue(b *testing.B) {
	q := NewQueue()
	item := QueueItem{
		FilePath: "/test/benchmark.mp3",
		Metadata: &meta.BookMetadata{
			Title:  "Benchmark Book",
			Author: "Benchmark Author",
		},
	}

	// Pre-populate queue
	for i := 0; i < b.N; i++ {
		q.Enqueue(item)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Dequeue()
	}
}

func BenchmarkEnqueueDequeue(b *testing.B) {
	q := NewQueue()
	item := QueueItem{
		FilePath: "/test/benchmark.mp3",
		Metadata: &meta.BookMetadata{
			Title:  "Benchmark Book",
			Author: "Benchmark Author",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Enqueue(item)
		q.Dequeue()
	}
}

func BenchmarkConcurrentAccess(b *testing.B) {
	q := NewQueue()
	item := QueueItem{
		FilePath: "/test/benchmark.mp3",
		Metadata: &meta.BookMetadata{
			Title:  "Benchmark Book",
			Author: "Benchmark Author",
		},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.Enqueue(item)
			q.Dequeue()
		}
	})
}
