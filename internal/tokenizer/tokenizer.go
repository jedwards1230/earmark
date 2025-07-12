package tokenizer

import (
	"fmt"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

func initTokenizer() (*tiktoken.Tiktoken, error) {
	encoding := "cl100k_base"
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tkm, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		err = fmt.Errorf("getEncoding: %v", err)
		return tkm, err
	}
	return tkm, err
}

// GetTokens returns raw tokens (probably not what you want for similarity search)
func GetTokens(content string) ([]int, error) {
	tkm, err := initTokenizer()
	if err != nil {
		err = fmt.Errorf("getEncoding: %v", err)
		return nil, err
	}

	// encode
	tokens := tkm.Encode(content, nil, nil)

	return tokens, nil
}

func DecodeTokens(tokens []int) (string, error) {
	tkm, err := initTokenizer()
	if err != nil {
		err = fmt.Errorf("getEncoding: %v", err)
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
