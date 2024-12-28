package transcribe

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/queue"
	"transcriber/internal/state"
)

type Transcriber struct {
	config       *config.Config
	stateManager *state.StateManager
	queue        *queue.Queue
	done         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewTranscriber(cfg *config.Config, sm *state.StateManager, q *queue.Queue) *Transcriber {
	if err := checkAndInstallWhisperCtranslate2(); err != nil {
		log.Fatalf("Error checking or installing whisper-ctranslate2: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Transcriber{
		config:       cfg,
		stateManager: sm,
		queue:        q,
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (t *Transcriber) StartWorker() {
	log.Println("Starting transcription worker...")
	for {
		select {
		case <-t.ctx.Done():
			log.Println("Transcriber context canceled, exiting worker.")
			close(t.done)
			return
		default:
			audioFilePath, ok := t.queue.Dequeue()
			if !ok {
				log.Println("Queue shutdown signal received, exiting worker.")
				close(t.done)
				return
			}
			if audioFilePath == "" {
				// No work available, wait a bit before checking again
				time.Sleep(time.Second)
				continue
			}

			// Process files sequentially
			t.transcribeAudio(audioFilePath)
			log.Printf("Finished processing file: %s", audioFilePath)
		}
	}
}

func (t *Transcriber) getRelativePath(audioFilePath string) string {
	// Find the "audiobooks" directory in the path
	audioBooksIndex := bytes.LastIndex([]byte(audioFilePath), []byte("/audiobooks/"))
	if audioBooksIndex == -1 {
		return "" // Return empty if "audiobooks" not found
	}

	// Get everything after "audiobooks/"
	relativePath := audioFilePath[audioBooksIndex+len("/audiobooks/"):]
	// Get the directory part of the relative path
	lastSlash := bytes.LastIndex([]byte(relativePath), []byte("/"))
	if lastSlash == -1 {
		return ""
	}
	return relativePath[:lastSlash]
}

func (t *Transcriber) ensureOutputDir(relativePath string) (string, error) {
	fullOutputDir := t.config.OutputDir
	if relativePath != "" {
		fullOutputDir = fmt.Sprintf("%s/%s", t.config.OutputDir, relativePath)
	}

	err := os.MkdirAll(fullOutputDir, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create output directory: %v", err)
	}
	return fullOutputDir, nil
}

func formatFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func (t *Transcriber) transcribeAudio(audioFilePath string) {
	if t.stateManager.IsProcessed(audioFilePath) {
		log.Printf("Skipping already processed file: %s", audioFilePath)
		return
	}

	fileInfo, err := os.Stat(audioFilePath)
	if err != nil {
		log.Printf("Failed to get file info: %v", err)
		return
	}
	log.Printf("Transcribing: %s (Size: %s)", audioFilePath, formatFileSize(fileInfo.Size()))

	// Get the relative path and create output directory
	relativePath := t.getRelativePath(audioFilePath)
	outputDir, err := t.ensureOutputDir(relativePath)
	if err != nil {
		log.Printf("Failed to create output directory: %v", err)
		return
	}

	threads := os.Getenv("WHISPER_THREADS")
	if threads == "" {
		threads = "1"
	}

	cmd := exec.Command(
		"whisper-ctranslate2",
		audioFilePath,
		"--model", t.config.WhisperModel,
		"--compute_type", "float32",
		"--language", "en",
		"--beam_size", "5",
		"--output_dir", outputDir,
		"--model_dir", t.config.ModelsDir,
		"--output_format", "txt",
		"--vad_filter", "True", // Voice Activity Detection - On by default when batched is True
		"--batched", "True",
		"--batch_size", "16",
		"--threads", threads,
		"--initial_prompt", "", // https://cookbook.openai.com/examples/whisper_prompting_guide
	)

	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Failed to run transcription command: %v\nStderr: %s", err, stderr.String())
		return
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		log.Printf("Audio file not found: %s", audioFilePath)
		return
	}

	if err := t.stateManager.MarkProcessed(audioFilePath); err != nil {
		log.Printf("Failed to mark file as processed: %v", err)
		return
	}
	log.Printf("Transcription completed for %s", audioFilePath)
}

func checkAndInstallWhisperCtranslate2() error {
	// Check if whisper-ctranslate2 is installed
	_, err := exec.LookPath("whisper-ctranslate2")
	if err == nil {
		log.Println("whisper-ctranslate2 is already installed.")
		return nil
	}

	// Attempt to install whisper-ctranslate2 using pip
	log.Println("whisper-ctranslate2 is not installed. Attempting to install it using pip...")
	cmd := exec.Command("pip", "install", "-U", "whisper-ctranslate2")
	installOutput, installErr := cmd.CombinedOutput()
	if installErr != nil {
		return fmt.Errorf("failed to install whisper-ctranslate2: %v\nOutput: %s", installErr, string(installOutput))
	}

	log.Println("whisper-ctranslate2 installed successfully.")
	return nil
}
