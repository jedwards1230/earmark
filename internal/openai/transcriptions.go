package openai

import (
	"context"
	"fmt"
	"transcriber/internal/config"

	"github.com/sashabaranov/go-openai"
)

type Transcriptions struct {
	c *openai.Client
}

func NewTranscriber(cfg *config.Config) *Transcriptions {
	return &Transcriptions{c: InitOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL)}
}

func (t *Transcriptions) GetTranscription(filepath string) (string, error) {
	resp, err := t.c.CreateTranscription(
		context.Background(),
		openai.AudioRequest{
			Model:    openai.Whisper1,
			FilePath: filepath,
		})
	if err != nil {
		return "", fmt.Errorf("transcription error: %v", err)
	}
	fmt.Println(resp.Text)

	return resp.Text, nil
}
