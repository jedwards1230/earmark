package worker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/meta"
	"transcriber/internal/queue"
	"transcriber/internal/state"
	"transcriber/internal/tokenizer"
	"transcriber/internal/transcribe"
)

type Worker struct {
	queue  *queue.Queue
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	db     *db.DB
}

type ProcessFunc func(context.Context, string)

func NewWorker(q *queue.Queue, db *db.DB) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		queue:  q,
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		db:     db,
	}
}

func (w *Worker) Start(cfg *config.Config, sm *state.StateManager) {
	log.Println("Worker started")
	for {
		select {
		case <-w.ctx.Done():
			log.Println("Worker received shutdown signal")
			close(w.done)
			return
		default:
			queueItem, ok := w.queue.Dequeue()
			if !ok || queueItem.FilePath == "" {
				// Instead of exiting, sleep briefly and try again
				time.Sleep(time.Second)
				continue
			}

			log.Printf("Processing file: %s (author: %s, title: %s)",
				queueItem.FilePath, queueItem.Metadata.Author, queueItem.Metadata.Title)

			transcriber := transcribe.NewTranscriber(cfg, sm)
			textFilePath, err := transcriber.TranscribeAudio(w.ctx, queueItem.FilePath)
			if err != nil {
				log.Printf("Transcription failed for %s: %v", queueItem.FilePath, err)
				continue
			}

			log.Printf("Successfully transcribed %s to %s", queueItem.FilePath, textFilePath)
			content, err := OpenFile(textFilePath)
			if err != nil {
				log.Printf("Failed to open transcribed file %s: %v", textFilePath, err)
				continue
			}

			log.Printf("Generating embedding for %s", queueItem.FilePath)
			embedding, err := tokenizer.GetEmbedding(content, cfg.OpenAIAPIKey)
			if err != nil {
				log.Printf("Failed to generate embedding for %s: %v", queueItem.FilePath, err)
				continue
			}
			log.Printf("Successfully generated embedding with %d dimensions", len(embedding))

			log.Printf("Storing embedding and metadata for %s", queueItem.FilePath)
			if err := w.db.StoreWithMetadata(
				w.ctx,
				embedding,
				content,
				meta.NewMetadata(
					queueItem.FilePath,
					queueItem.Metadata.Author,
					queueItem.Metadata.Title,
					"",
					queueItem.Metadata.ISBN,
				),
			); err != nil {
				log.Printf("Failed to store embedding for %s: %v", queueItem.FilePath, err)
				continue
			}
			log.Printf("Successfully processed %s", queueItem.FilePath)
		}
	}
}

func (w *Worker) Run(process ProcessFunc) {
}

func (w *Worker) Stop() {
	w.cancel()
	<-w.done
}

func OpenFile(filepath string) (string, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", fmt.Errorf("opening file: %v", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("reading file: %v", err)
	}

	content := string(data)

	return content, nil
}
