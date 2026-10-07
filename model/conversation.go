package model

const (
	TitleSourceUser = "user"
	TitleSourceLLM  = "llm"
)

type Conversation struct {
	Id          string
	Title       string
	TitleSource string
	Summary     string
	Messages    []Message
}
