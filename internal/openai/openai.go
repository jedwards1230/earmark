package openai

import (
	"github.com/sashabaranov/go-openai"
)

func InitOpenAI(authToken, baseUrl string) *openai.Client {
	config := openai.DefaultConfig(authToken)
	config.BaseURL = "https://api.openai.com/v1"
	client := openai.NewClientWithConfig(config)
	return client
}
