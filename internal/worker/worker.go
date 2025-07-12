package worker

import (
	"context"
	"fmt"
	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/correction"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/queue"
	"github.com/jedwards1230/lil-whisper/internal/transcribe"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

type Worker struct {
	queue       *queue.Queue
	done        chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	db          *db.DB
	corrector   correction.Corrector
	fileManager *correction.FileManager
	log         log.Logger
}

func NewWorker(q *queue.Queue, db *db.DB, cfg *config.Config) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	logger := log.NewLogger("worker")

	// Initialize LLM corrector
	corrector := correction.New(cfg)

	// Initialize file manager for dual text storage
	fileManager := correction.NewFileManager(cfg)

	return &Worker{
		queue:       q,
		done:        make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
		db:          db,
		corrector:   corrector,
		fileManager: fileManager,
		log:         logger,
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

			// Get file info for transcription storage
			fileInfo, err := os.Stat(queueItem.FilePath)
			if err != nil {
				w.log.Error("Failed to get file info",
					"file", queueItem.FilePath,
					"error", err)
				continue
			}

			content, err := w.transcribeFile(cfg, queueItem, fileMeta, startTime, fileInfo.Size())
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

			// Save raw transcription text to local file
			if err := w.fileManager.SaveRawText(queueItem.FilePath, content); err != nil {
				w.log.Warn("Failed to save raw text file", "error", err)
				// Don't fail the entire process for file save errors
			}

			// LLM Text Correction step
			correctionStart := time.Now()
			finalContent := content // Default to original content

			if w.corrector.IsEnabled() {
				w.log.Debug("Starting LLM text correction",
					"file", queueItem.FilePath,
					"chapter", fileMeta.Chapter)

				// Use atomic correction processing
				err := w.db.ProcessTranscriptionCorrection(w.ctx, queueItem.FilePath, func() (string, map[string]interface{}, error) {
					// This function runs the actual correction
					correctionResult, err := w.corrector.CorrectText(w.ctx, content, fileMeta)
					if err != nil {
						return "", nil, err
					}
					return correctionResult.CorrectedText, correctionResult.Metadata, nil
				})

				if err != nil {
					w.log.Error("LLM correction failed, using original text",
						"chapter", fileMeta.Chapter,
						"title", queueItem.Metadata.Title,
						"error", err)
					// finalContent remains as original content
				} else {
					// Get the corrected text from database
					transcription, err := w.db.GetTranscription(w.ctx, queueItem.FilePath)
					if err != nil {
						w.log.Error("Failed to retrieve corrected text", "error", err)
						// Use original content as fallback
					} else if transcription.CorrectedText != nil && *transcription.CorrectedText != "" {
						finalContent = *transcription.CorrectedText
						correctionDuration := time.Since(correctionStart).Round(time.Millisecond)

						w.log.Info("LLM correction completed",
							"chapter", fileMeta.Chapter,
							"title", queueItem.Metadata.Title,
							"correction_duration", correctionDuration)

						// Save corrected text to local file
						if err := w.fileManager.SaveCorrectedText(queueItem.FilePath, finalContent); err != nil {
							w.log.Warn("Failed to save corrected text file", "error", err)
							// Don't fail the entire process for file save errors
						}
					}
				}
			} else {
				w.log.Debug("LLM correction disabled, using original text")
			}

			// Use corrected content (or original if correction failed/disabled) for chunking and embedding
			if err := w.db.InsertContentWithMetadata(w.ctx, finalContent, fileMeta); err != nil {
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

func (w *Worker) transcribeFile(cfg *config.Config, queueItem queue.QueueItem, fileMeta *meta.FileMetadata, startTime time.Time, fileSize int64) (string, error) {
	// Check if transcription already exists and is up to date
	fileChecksum, err := w.db.ComputeFileChecksum(queueItem.FilePath)
	if err != nil {
		w.log.Warn("Failed to compute file checksum, proceeding with transcription", "file", queueItem.FilePath, "error", err)
	} else {
		settingsHash := w.db.ComputeSettingsHash(cfg)
		needsTranscription, err := w.db.NeedsTranscription(w.ctx, queueItem.FilePath, fileChecksum, settingsHash)
		if err != nil {
			w.log.Warn("Failed to check transcription status, proceeding with transcription", "file", queueItem.FilePath, "error", err)
		} else if !needsTranscription {
			// Transcription exists and is up to date, retrieve it
			transcription, err := w.db.GetTranscription(w.ctx, queueItem.FilePath)
			if err != nil {
				w.log.Warn("Failed to retrieve existing transcription, proceeding with new transcription", "file", queueItem.FilePath, "error", err)
			} else {
				w.log.Info("Using existing transcription", "file", queueItem.FilePath, "word_count", transcription.WordCount)
				return transcription.TranscriptionText, nil
			}
		}
	}

	transcriber := transcribe.NewTranscriber(cfg)
	textFilePath, err := transcriber.TranscribeAudio(w.ctx, queueItem.FilePath, fileMeta)
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

	// Clean up the temporary Yap output file since we now save raw text via FileManager
	if err := os.Remove(textFilePath); err != nil {
		w.log.Warn("Failed to clean up temporary transcription file", "file", textFilePath, "error", err)
		// Don't fail the entire process for cleanup errors
	} else {
		w.log.Debug("Cleaned up temporary transcription file", "file", textFilePath)
	}

	// Calculate processing duration
	endTime := time.Now()
	processingDuration := endTime.Sub(startTime)
	processingDurationMs := processingDuration.Milliseconds()

	// Count words in transcription
	wordCount := w.countWords(content)

	// Store raw transcription in database
	if fileChecksum != "" {
		settingsHash := w.db.ComputeSettingsHash(cfg)
		err = w.db.StoreTranscription(w.ctx, queueItem.FilePath, fileChecksum, settingsHash, content, fileSize, wordCount, processingDurationMs)
		if err != nil {
			w.log.Error("Failed to store transcription in database",
				"file", queueItem.FilePath,
				"error", err)
			// Don't fail the entire process, just log the error
		} else {
			w.log.Debug("Stored raw transcription",
				"file", queueItem.FilePath,
				"word_count", wordCount,
				"duration_ms", processingDurationMs)
		}
	}

	return content, nil
}

func (w *Worker) Stop() {
	w.cancel()
	<-w.done
}

// countWords counts the number of words in a string
func (w *Worker) countWords(content string) int {
	re := regexp.MustCompile(`\S+`)
	return len(re.FindAllString(content, -1))
}

func OpenFile(filePath string) (string, error) {
	// #nosec G304 - filePath is controlled by caller and validated elsewhere
	file, err := os.Open(filepath.Clean(filePath))
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
