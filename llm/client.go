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
	"git.estatecloud.org/radumaco/souvenir/model"
)

const COMPLETIONS_API = "/v1/chat/completions"
const MODELS_API = "/v1/models"

type LLMClient struct {
	Cfg    config.Config
	Logger slog.Logger
}

// Request Types

type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []model.Message `json:"messages"`
	Tools    []config.Tool   `json:"tools,omitempty"`
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
	Index        int           `json:"index"`
	Message      model.Message `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (client LLMClient) Models() ([]string, error) {
	resp, err := http.Get(client.Cfg.Api.Url + MODELS_API)
	if err != nil {
		client.Logger.Error("There was an error during the network request", "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		client.Logger.Error("There was an error reading the response body", "error", err.Error())
		return nil, err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		client.Logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return nil,
			errors.New("Call returned invalid status " +
				strconv.Itoa(resp.StatusCode) +
				resp.Status)
	}
	client.Logger.Debug("Received data", "data", data)

	var response struct{
		Data []struct{
			Id string
		}
	}
	if err := json.Unmarshal(data, &response); err != nil {
		client.Logger.Error("There was an error parsing the response", "error", err.Error())
		return nil, err
	}
	client.Logger.Debug("Unmarshal response", "data", response)
	var ret []string
	for _, m := range response.Data {
		ret = append(ret, m.Id)
	}
	return ret, nil
}

func (client LLMClient) Call(query string, messages []model.Message) ([]model.Message, error) {
	requestBody := ChatRequest{
		Model:    client.Cfg.Llm.Model,
		Messages: messages,
		Tools:    config.AvailableTools(),
	}
	serialized, err := json.Marshal(requestBody)
	if err != nil {
		return []model.Message{}, err
	}

	body := bytes.NewBuffer(serialized)
	req, err := http.NewRequest("POST", client.Cfg.Api.Url + COMPLETIONS_API, body)
	if err != nil {
		client.Logger.Error("There was an error creating the request", "error", err.Error())
		return []model.Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if client.Cfg.Api.Key != "" {
		req.Header.Set("Authorization", "Bearer "+client.Cfg.Api.Key)
	}
	client.Logger.Debug("Executing llm call", "req", req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		client.Logger.Error("There was an error during http call", "error", err.Error())
		return []model.Message{}, err
	}
	client.Logger.Debug("LLM Call returned", "response", resp)
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		client.Logger.Error("There was an error reading the response body", "error", err.Error())
		return []model.Message{}, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		client.Logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return []model.Message{},
			errors.New("Call returned invalid status " +
				strconv.Itoa(resp.StatusCode) +
				resp.Status)
	}

	client.Logger.Debug("Received data", "data", data)
	var response ChatResponse
	if err := json.Unmarshal(data, &response); err != nil {
		client.Logger.Error("There was an error parsing the response", "error", err.Error())
		return []model.Message{}, err
	}

	var result []model.Message
	if len(response.Choices) == 0 {
		client.Logger.Error("The call returned an empty response")
		return []model.Message{}, errors.New("The call returned an empty response")
	}
	for _, choice := range response.Choices {
		client.Logger.Error("Appending response message", "message", choice.Message)
		result = append(result, choice.Message)
	}

	return result, nil
}
