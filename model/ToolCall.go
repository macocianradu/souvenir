package model

type ToolCall struct {
	Id                string `json:"-"`
	Type              string `json:"type"`
	FunctionName      string `json:"function"`
	FunctionArguments string `json:"arguments"`
}
