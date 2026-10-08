package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	config "git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
)

const COMPLETIONS_API = "/v1/chat/completions"
const MODELS_API = "/v1/models"

type ChatClient struct {
	Cfg     config.Config
	logger  slog.Logger
	client  *http.Client
	timeout time.Duration
}

// Request Types

type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []model.Message `json:"messages"`
	Tools    []ToolSpec      `json:"tools,omitempty"`
	Stream   bool            `json:"stream,omitempty"`
	Kwargs   *Kwargs         `json:"chat_template_kwargs,omitempty"`
}

type Kwargs struct {
	Thinking bool `json:"enable_thinking"`
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
	timeout := time.Duration(cfg.Api.Timeout) * time.Millisecond
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	return &ChatClient{
		Cfg:     cfg,
		logger:  *slog.Default().With("Component", "ChatClient"),
		client:  &http.Client{Transport: transport},
		timeout: timeout,
	}
}

func (cl ChatClient) templateKwargs(allow bool) *Kwargs {
	if cl.Cfg.Llm.Thinking == nil {
		return nil
	}
	return &Kwargs{Thinking: allow && *cl.Cfg.Llm.Thinking}
}

func (cl ChatClient) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cl.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", cl.Cfg.Api.Url+MODELS_API, nil)
	if err != nil {
		cl.logger.Error("There was an error creating the request", "error", err.Error())
		return nil, err
	}
	resp, err := cl.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response struct {
		Data []struct {
			Id string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		cl.logger.Error("There was an error parsing the response", "error", err.Error())
		return nil, err
	}
	var ret []string
	for _, m := range response.Data {
		ret = append(ret, m.Id)
	}
	return ret, nil
}

func (cl ChatClient) Rename(ctx context.Context, messages []model.Message) (RenameResponse, error) {
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
		Kwargs: cl.templateKwargs(false),
	}
	request.Messages = append(request.Messages, model.Message{
		Role:    "user",
		Content: "Give me a title and description for this conversation. Reply with just a json and nothing else. The format is exactly {\"title\":<title>, \"summary\":<summary>}. Add nothing. Change nothing",
	})
	cl.logger.Debug("Calling rename", "model", llm, "messages", len(request.Messages))
	out, err := cl.Call(ctx, request)
	if err != nil {
		cl.logger.Error("Error while asking for rename", "error", err)
		return RenameResponse{}, err
	}
	return parseRename(out[0].Content)
}

func parseRename(content string) (RenameResponse, error) {
	var meta RenameResponse
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start < 0 || end < start {
		return meta, fmt.Errorf("Rename reply contains no JSON object: %q", content)
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &meta); err != nil {
		return meta, fmt.Errorf("Could not parse rename reply: %w", err)
	}
	if strings.TrimSpace(meta.Title) == "" {
		return meta, errors.New("Rename reply has an empty title")
	}
	return meta, nil
}

func (cl ChatClient) Call(ctx context.Context, requestBody ChatRequest) ([]model.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, cl.timeout)
	defer cancel()
	resp, err := cl.CallApi(ctx, requestBody)
	if err != nil {
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
		return nil, err
	}

	if len(response.Choices) == 0 {
		cl.logger.Error("The call returned an empty response")
		return nil, errors.New("The call returned an empty response")
	}
	var result []model.Message
	for _, choice := range response.Choices {
		result = append(result, choice.Message)
	}
	return result, nil
}

func (cl ChatClient) CallApi(ctx context.Context, requestBody ChatRequest) (*http.Response, error) {
	serialized, err := json.Marshal(requestBody)
	if err != nil {
		cl.logger.Error("There was an error marshelling the request body", "error", err)
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cl.Cfg.Api.Url+COMPLETIONS_API, bytes.NewReader(serialized))
	if err != nil {
		cl.logger.Error("There was an error creating the request", "error", err.Error())
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	cl.logger.Debug("Executing llm call", "model", requestBody.Model, "stream", requestBody.Stream)
	return cl.do(req)
}

func (cl ChatClient) do(req *http.Request) (*http.Response, error) {
	if cl.Cfg.Api.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cl.Cfg.Api.Key)
	}
	resp, err := cl.client.Do(req)
	if err != nil {
		cl.logger.Error("There was an error during http call", "error", err.Error())
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		cl.logger.Error("Call returned non 200 status", "status", resp.Status, "body", string(body))
		return nil, fmt.Errorf("Call returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}
