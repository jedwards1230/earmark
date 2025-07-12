package tokenizer

import (
	"fmt"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

var (
	tokenizer     *tiktoken.Tiktoken
	tokenizerOnce sync.Once
	tokenizerErr  error
)

func getTokenizer() (*tiktoken.Tiktoken, error) {
	tokenizerOnce.Do(func() {
		encoding := "cl100k_base"
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		tokenizer, tokenizerErr = tiktoken.GetEncoding(encoding)
		if tokenizerErr != nil {
			tokenizerErr = fmt.Errorf("getEncoding: %v", tokenizerErr)
		}
	})
	return tokenizer, tokenizerErr
}

// GetTokens returns raw tokens (probably not what you want for similarity search)
func GetTokens(content string) ([]int, error) {
	tkm, err := getTokenizer()
	if err != nil {
		return nil, err
	}

	// encode
	tokens := tkm.Encode(content, nil, nil)

	return tokens, nil
}

func DecodeTokens(tokens []int) (string, error) {
	tkm, err := getTokenizer()
	if err != nil {
		return "", err
	}

	// decode
	content := tkm.Decode(tokens)

	return content, nil
}

// CountTokens returns the number of tokens in the given text
func CountTokens(content string) (int, error) {
	tokens, err := GetTokens(content)
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}
