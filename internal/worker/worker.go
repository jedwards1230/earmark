package worker

import (
	"context"
	"fmt"
	"io"
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
	for {
		select {
		case <-w.ctx.Done():
			close(w.done)
			return
		default:
			filePath, ok := w.queue.Dequeue()
			if !ok {
				close(w.done)
				return
			}
			if filePath == "" {
				time.Sleep(time.Second)
				continue
			}

			transcriber := transcribe.NewTranscriber(cfg, sm)
			textFilePath, err := transcriber.TranscribeAudio(w.ctx, filePath)
			if err != nil {
				fmt.Printf("Failed to transcribe %s: %v\n", filePath, err)
				continue
			}

			fmt.Printf("Transcribed %s to %s\n", filePath, textFilePath)
			content, err := OpenFile(textFilePath)
			if err != nil {
				fmt.Printf("Failed to open file: %v\n", err)
				continue
			}

			embedding, err := tokenizer.GetEmbedding(content, cfg.OpenAIAPIKey)
			if err != nil {
				fmt.Printf("Failed to get embedding: %v\n", err)
				continue
			}
			fmt.Printf("Generated embedding with %d dimensions\n", len(embedding))

			fmt.Printf("Storing embedding for %s\n", filePath)
			metadata := meta.NewMetadata(filePath, "", "", "", "")
			w.db.StoreWithMetadata(w.ctx, embedding, content, metadata)
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
