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
	"transcriber/internal/openai"
	"transcriber/internal/queue"
	"transcriber/internal/transcribe"
)

var useNewTranscriber = false

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

func (w *Worker) Start(cfg *config.Config) {
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
				time.Sleep(time.Second)
				continue
			}

			// Find matching FileMetadata
			var fileMeta *meta.FileMetadata
			for _, fm := range queueItem.Metadata.FileMetas {
				if fm.FilePath == queueItem.FilePath {
					fileMeta = &fm
					break
				}
			}

			if fileMeta == nil {
				log.Printf("No metadata found for file: %s", queueItem.FilePath)
				continue
			}

			log.Printf("Processing '%s' by %s - Chapter %d: %s",
				queueItem.Metadata.Title, queueItem.Metadata.Author, fileMeta.ChapterIndex, fileMeta.Chapter)

			startTime := time.Now()
			content, err := w.transcribeFile(cfg, queueItem, fileMeta)
			if err != nil {
				log.Printf("Failed to transcribe Chapter %s of '%s': %v",
					fileMeta.Chapter, queueItem.Metadata.Title, err)
				continue
			}
			endTime := time.Now()
			duration := endTime.Sub(startTime)
			log.Printf("Transcription of Chapter %s of '%s' took %s",
				fileMeta.Chapter, queueItem.Metadata.Title, duration)

			if err := w.db.InsertContentWithMetadata(w.ctx, content, fileMeta); err != nil {
				log.Printf("Failed to store Chapter %s of '%s': %v",
					fileMeta.Chapter, queueItem.Metadata.Title, err)
				continue
			}

			log.Printf("Completed Chapter %s of '%s'",
				fileMeta.Chapter, queueItem.Metadata.Title)
		}
	}
}

func (w *Worker) transcribeFile(cfg *config.Config, queueItem queue.QueueItem, fileMeta *meta.FileMetadata) (string, error) {
	if useNewTranscriber {
		return w.newTranscriber(cfg, queueItem)
	}
	return w.oldTranscriber(cfg, queueItem, fileMeta)
}

func (w *Worker) oldTranscriber(cfg *config.Config, queueItem queue.QueueItem, fileMeta *meta.FileMetadata) (string, error) {
	transcriber := transcribe.NewTranscriber(cfg)
	textFilePath, err := transcriber.TranscribeAudio(w.ctx, queueItem.FilePath, fileMeta, cfg.WhisperThreads, cfg.WhisperComputeType)
	if err != nil {
		log.Printf("Failed to transcribe Chapter %s of '%s': %v",
			fileMeta.Chapter, queueItem.Metadata.Title, err)
		return "", err
	}

	content, err := OpenFile(textFilePath)
	if err != nil {
		log.Printf("Failed to read Chapter %s of '%s': %v",
			fileMeta.Chapter, queueItem.Metadata.Title, err)
		return "", err
	}

	return content, nil
}

func (w *Worker) newTranscriber(cfg *config.Config, queueItem queue.QueueItem) (string, error) {
	transcriber := openai.NewTranscriber(cfg)
	content, err := transcriber.GetTranscription(queueItem.FilePath)
	return content, err
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
