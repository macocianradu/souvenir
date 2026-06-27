package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	config "git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
)

const COMPLETIONS_API = "/v1/chat/completions"
const MODELS_API = "/v1/models"

type ChatClient struct {
	Cfg    config.Config
	logger slog.Logger
	client *http.Client
}

// Request Types

type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []model.Message `json:"messages"`
	Tools    []config.Tool   `json:"tools,omitempty"`
	Stream   bool            `json:"stream,omitempty"`
	Kwargs   Kwargs          `json:"chat_template_kwargs"`
}

type Kwargs struct {
	Thinking bool `json:"enable_thinking"`
}

type Reasoning struct {
	Effort string `json:"effort"`
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

type RenameResponse struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
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

func NewLLMClient(cfg config.Config) *ChatClient {
	return &ChatClient{
		Cfg:    cfg,
		logger: *slog.Default().With("Component", "ChatClient"),
		client: &http.Client{Timeout: time.Duration(cfg.Api.Timeout) * time.Millisecond},
	}
}

func (cl ChatClient) Models() ([]string, error) {
	req, err := http.NewRequest("GET", cl.Cfg.Api.Url+MODELS_API, nil)
	if err != nil {
		cl.logger.Error("There was an error creating the request", "error", err.Error())
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cl.Cfg.Api.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cl.Cfg.Api.Key)
	}
	resp, err := cl.client.Do(req)
	if err != nil {
		cl.logger.Error("There was an error during the network request", "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		cl.logger.Error("There was an error reading the response body", "error", err.Error())
		return nil, err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		cl.logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return nil,
			errors.New("Call returned invalid status " +
				strconv.Itoa(resp.StatusCode) +
				resp.Status)
	}
	cl.logger.Debug("Received data", "data", data)

	var response struct {
		Data []struct {
			Id string
		}
	}
	if err := json.Unmarshal(data, &response); err != nil {
		cl.logger.Error("There was an error parsing the response", "error", err.Error())
		return nil, err
	}
	cl.logger.Debug("Unmarshal response", "data", response)
	var ret []string
	for _, m := range response.Data {
		ret = append(ret, m.Id)
	}
	return ret, nil
}

func (cl ChatClient) Rename(messages []model.Message) (RenameResponse, error) {
	llm := cl.Cfg.Llm.TitleModel
	if llm == "" {
		llm = cl.Cfg.Llm.Model
	}
	request := ChatRequest{
		Model: llm,
		Messages: append([]model.Message{{
			Role:    "system",
			Content: "Reply with ONLY JSON with the form: {\"title\":<title>, \"summary\":<summary>}. Title <= 10 words. Summary 2-3 sentances.",
		}}, messages...),
		Kwargs: Kwargs{Thinking: false},
	}
	request.Messages = append(request.Messages, model.Message{
		Role:    "user",
		Content: "Give me a title and description for this conversation. Reply with just a json and nothing else. The format is exactly {\"title\":<title>, \"summary\":<summary>}. Add nothing. Change nothing",
	})
	cl.logger.Debug("Calling rename", "request", request)
	out, err := cl.Call(request)
	if err != nil {
		cl.logger.Error("Error while asking for rename", "error", err)
		return RenameResponse{}, err
	}
	var meta RenameResponse
	cl.logger.Debug("Received rename answer", "content", out[0].Content)
	if idx := strings.IndexByte(out[0].Content, '{'); idx >= 0 {
		if lidx := strings.LastIndexByte(out[0].Content, '}'); lidx <= len(out[0].Content) {
			trimmed := out[0].Content[idx:lidx + 1]
			cl.logger.Debug("Trimmed answer", "content", trimmed)
			if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
				cl.logger.Error("Could not parse the rename response", "content", out[0].Content)
				return meta, err
			}
			return meta, nil
		}
	}
	cl.logger.Error("Response is not standard json", "content", out[0].Content)
	return meta, errors.New("Response is not standard json")
}

func (cl ChatClient) Query(messages []model.Message) ([]model.Message, error) {
	requestBody := ChatRequest{
		Model:    cl.Cfg.Llm.Model,
		Messages: messages,
		Tools:    config.AvailableTools(),
		Stream:   true,
		Kwargs:   Kwargs{Thinking: false},
	}
	return cl.Call(requestBody)
}

func (cl ChatClient) Call(requestBody ChatRequest) ([]model.Message, error) {
	resp, err := cl.CallApi(requestBody)

	if err != nil {
		cl.logger.Error("There was an error during the API call", "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		cl.logger.Error("There was an error reading the response body", "error", err.Error())
		return nil, err
	}

	cl.logger.Debug("Received data", "data", data)
	var response ChatResponse
	if err := json.Unmarshal(data, &response); err != nil {
		cl.logger.Error("There was an error parsing the response", "error", err.Error())
		return []model.Message{}, err
	}

	var result []model.Message
	if len(response.Choices) == 0 {
		cl.logger.Error("The call returned an empty response")
		return []model.Message{}, errors.New("The call returned an empty response")
	}
	for _, choice := range response.Choices {
		cl.logger.Debug("Appending response message", "message", choice.Message)
		result = append(result, choice.Message)
	}

	return result, nil
}

func (cl ChatClient) CallApi(requestBody ChatRequest) (*http.Response, error) {
	serialized, err := json.Marshal(requestBody)
	if err != nil {
		cl.logger.Error("There was an error marshelling the request body",
			"request", requestBody,
			"error", err)
		return nil, err
	}

	body := bytes.NewBuffer(serialized)
	req, err := http.NewRequest("POST", cl.Cfg.Api.Url+COMPLETIONS_API, body)
	if err != nil {
		cl.logger.Error("There was an error creating the request", "error", err.Error())
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cl.Cfg.Api.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cl.Cfg.Api.Key)
	}
	cl.logger.Debug("Executing llm call")
	resp, err := cl.client.Do(req)
	if err != nil {
		cl.logger.Error("There was an error during http call", "error", err.Error())
		return nil, err
	}
	cl.logger.Debug("LLM Call returned", "response", resp.Status)

	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		cl.logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return nil,
			errors.New("Call returned invalid status " +
				strconv.Itoa(resp.StatusCode) +
				resp.Status)
	}
	return resp, nil
}
