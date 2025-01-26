package worker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/meta"
	"transcriber/internal/queue"
	"transcriber/internal/transcribe"
)

type Worker struct {
	queue  *queue.Queue
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	db     *db.DB
	log    *log.Logger
}

func NewWorker(q *queue.Queue, db *db.DB) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	logger := log.New(os.Stdout, "(worker) ", 0)
	return &Worker{
		queue:  q,
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		db:     db,
		log:    logger,
	}
}

func (w *Worker) Start(cfg *config.Config) {
	w.log.Println("Worker started")
	for {
		select {
		case <-w.ctx.Done():
			w.log.Println("Worker received shutdown signal")
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
				w.log.Printf("No metadata found for file: %s", queueItem.FilePath)
				continue
			}

			w.log.Printf("Processing '%s' by %s - Chapter %d: %s",
				queueItem.Metadata.Title, queueItem.Metadata.Author, fileMeta.ChapterIndex, fileMeta.Chapter)

			startTime := time.Now()
			content, err := w.transcribeFile(cfg, queueItem, fileMeta)
			if err != nil {
				w.log.Printf("Failed to transcribe Chapter %s of '%s': %v",
					fileMeta.Chapter, queueItem.Metadata.Title, err)
				continue
			}
			endTime := time.Now()
			duration := endTime.Sub(startTime)
			duration = duration.Round(time.Second)
			w.log.Printf("Transcription of Chapter %s of '%s' took %s",
				fileMeta.Chapter, queueItem.Metadata.Title, duration)

			if err := w.db.InsertContentWithMetadata(w.ctx, content, fileMeta); err != nil {
				w.log.Printf("Failed to store Chapter %s of '%s': %v",
					fileMeta.Chapter, queueItem.Metadata.Title, err)
				continue
			}

			w.log.Printf("Completed Chapter %s of '%s'",
				fileMeta.Chapter, queueItem.Metadata.Title)
		}
	}
}

func (w *Worker) transcribeFile(cfg *config.Config, queueItem queue.QueueItem, fileMeta *meta.FileMetadata) (string, error) {
	transcriber := transcribe.NewTranscriber(cfg)
	textFilePath, err := transcriber.TranscribeAudio(w.ctx, queueItem.FilePath, fileMeta, cfg.WhisperThreads, cfg.WhisperComputeType)
	if err != nil {
		baseErr := fmt.Sprintf("Transcription failed for %q (Chapter %q)",
			queueItem.FilePath, fileMeta.Chapter)
		if exitErr, ok := err.(*exec.ExitError); ok {
			baseErr += fmt.Sprintf(" - Exit Code: %d", exitErr.ExitCode())
			if exitErr.Sys() != nil {
				baseErr += fmt.Sprintf(" - System Error: %v", exitErr.Sys())
			}
		}
		w.log.Printf("Error: %s - %v", baseErr, err)
		return "", fmt.Errorf("%s: %w", baseErr, err)
	}

	content, err := OpenFile(textFilePath)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to read output file %q for Chapter %q of %q",
			textFilePath, fileMeta.Chapter, queueItem.Metadata.Title)
		w.log.Printf("%s: %v", errMsg, err)
		return "", fmt.Errorf("%s: %w", errMsg, err)
	}

	return content, nil
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
