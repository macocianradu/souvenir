package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	config "git.estatecloud.org/radumaco/souvenir/config"
)

type LLMClient struct {
	Cfg config.Config
}

// Request Types

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []Message     `json:"messages"`
	Tools    []config.Tool `json:"tools,omitempty"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Response types //

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (client LLMClient) Call(query string, messages []Message) ([]Message, error) {
	var logger = slog.Default().With("Component", "LLM Client")
	requestBody := ChatRequest{
		Model:    client.Cfg.Llm.Model,
		Messages: messages,
		Tools:    config.AvailableTools(),
	}
	serialized, err := json.Marshal(requestBody)
	if err != nil {
		return []Message{}, err
	}

	body := bytes.NewBuffer(serialized)
	logger.Debug("Executing llm call", "query", query, "messages", messages, "body", serialized)
	req, err := http.NewRequest("POST", client.Cfg.Api.Url, body)
	if err != nil {
		logger.Error("There was an error creating the request", "error", err.Error())
		return []Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+client.Cfg.Api.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Error("There was an error during http call", "error", err.Error())
		return []Message{}, err
	}
	logger.Debug("LLM Call returned", "response", resp)
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("There was an error reading the response body", "error", err.Error())
		return []Message{}, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return []Message{},
			errors.New("Call returned invalid status " +
				strconv.Itoa(resp.StatusCode) +
				resp.Status)
	}

	logger.Debug("Received data", "data", data)
	var response ChatResponse
	if err := json.Unmarshal(data, &response); err != nil {
		logger.Error("There was an error parsing the response", "error", err.Error())
		return []Message{}, err 
	}

	var result []Message
	if len(response.Choices) == 0 {
		logger.Error("The call returned an empty response")
		return []Message{}, errors.New("The call returned an empty response")
	}
	for _, choice := range response.Choices {
		logger.Error("Appending response message", "message", choice.Message)
		result = append(result, choice.Message)
	}

	return result, nil
}
