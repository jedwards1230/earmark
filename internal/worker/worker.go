package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/log"
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
	log    log.Logger
}

func NewWorker(q *queue.Queue, db *db.DB) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	logger := log.NewLogger("worker")
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
	w.log.Info("Worker started", "status", "started")
	for {
		select {
		case <-w.ctx.Done():
			w.log.Info("Worker received shutdown signal", "status", "shutdown")
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
				w.log.Debug("No metadata found for file", "path", queueItem.FilePath)
				continue
			}

			w.log.Info("Processing chapter",
				"title", queueItem.Metadata.Title,
				"author", queueItem.Metadata.Author,
				"chapter_index", fileMeta.ChapterIndex,
				"chapter_name", fileMeta.Chapter)

			startTime := time.Now()
			content, err := w.transcribeFile(cfg, queueItem, fileMeta)
			if err != nil {
				w.log.Error("Failed to transcribe chapter",
					"chapter", fileMeta.Chapter,
					"title", queueItem.Metadata.Title,
					"error", err)
				continue
			}
			endTime := time.Now()
			duration := endTime.Sub(startTime).Round(time.Second)
			w.log.Info("Transcription completed",
				"chapter", fileMeta.Chapter,
				"title", queueItem.Metadata.Title,
				"duration", duration)

			if err := w.db.InsertContentWithMetadata(w.ctx, content, fileMeta); err != nil {
				w.log.Error("Failed to store chapter",
					"chapter", fileMeta.Chapter,
					"title", queueItem.Metadata.Title,
					"error", err)
				continue
			}

			w.log.Info("Chapter completed",
				"chapter", fileMeta.Chapter,
				"title", queueItem.Metadata.Title)
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
			w.log.Error("Transcription process error",
				"file", queueItem.FilePath,
				"chapter", fileMeta.Chapter,
				"exit_code", exitErr.ExitCode(),
				"system_error", exitErr.Sys(),
				"error", err)
		}
		return "", fmt.Errorf("%s: %w", baseErr, err)
	}

	content, err := OpenFile(textFilePath)
	if err != nil {
		w.log.Error("Failed to read output file",
			"file", textFilePath,
			"chapter", fileMeta.Chapter,
			"title", queueItem.Metadata.Title,
			"error", err)
		return "", fmt.Errorf("failed to read output file %q: %w", textFilePath, err)
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
