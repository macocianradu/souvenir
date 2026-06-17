package model

type Conversation struct {
	Id       string
	Name     string
	Summary  string
	Messages []Message
}
