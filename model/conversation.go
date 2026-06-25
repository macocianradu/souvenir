package model

type Conversation struct {
	Id       string
	Title    string
	Summary  string
	Messages []Message
}
