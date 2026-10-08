package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/model"
)

type StreamEvent struct {
	Reasoning string
	Content   string
	Messages  []model.Message
	Done      bool
	Err       error
}

type ChatStreamResponse struct {
	Choices []StreamChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type StreamChoice struct {
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type StreamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int    `json:"index"`
	Id       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func (cl ChatClient) QueryStream(ctx context.Context, messages []model.Message, tools []ToolSpec) (<-chan StreamEvent, error) {
	requestBody := ChatRequest{
		Model:    cl.Cfg.Llm.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Kwargs:   cl.templateKwargs(true),
	}
	return cl.CallStream(ctx, requestBody)
}

// CallStream starts a streaming request. Cancelling ctx aborts the request
// and closes the returned channel.
func (cl ChatClient) CallStream(ctx context.Context, requestBody ChatRequest) (<-chan StreamEvent, error) {
	resp, err := cl.CallApi(ctx, requestBody)
	if err != nil {
		return nil, err
	}
	events := make(chan StreamEvent)
	go cl.readStream(ctx, resp, events)
	return events, nil
}

func (cl ChatClient) readStream(ctx context.Context, resp *http.Response, events chan<- StreamEvent) {
	cl.logger.Debug("Started readStream")
	defer resp.Body.Close()
	defer close(events)

	// send gives up once ctx is done, so the goroutine never blocks on a
	// reader that has gone away.
	send := func(ev StreamEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var content strings.Builder
	toolAcc := map[int]*model.ToolCall{}
	var order []int

	for scanner.Scan() {
		line := scanner.Text()
		cl.logger.Debug("Stream got new line", "line", line)

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			cl.logger.Debug("Stream done")
			break
		}

		var chunk ChatStreamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			cl.logger.Error("Error parsing stream chunk", "payload", payload, "error", err)
			send(StreamEvent{Err: err})
			return
		}
		if chunk.Error != nil {
			cl.logger.Error("Stream reported an error", "message", chunk.Error.Message)
			send(StreamEvent{Err: errors.New(chunk.Error.Message)})
			return
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if reasoning := delta.ReasoningContent + delta.Reasoning; reasoning != "" {
			if !send(StreamEvent{Reasoning: reasoning}) {
				return
			}
		}
		if delta.Content != "" {
			content.WriteString(delta.Content)
			if !send(StreamEvent{Content: delta.Content}) {
				return
			}
		}

		for _, tc := range delta.ToolCalls {
			acc, ok := toolAcc[tc.Index]
			if !ok {
				acc = &model.ToolCall{Type: "function"}
				toolAcc[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.Id != "" {
				acc.Id = tc.Id
			}
			if tc.Type != "" {
				acc.Type = tc.Type
			}
			if tc.Function.Name != "" {
				acc.Function.Name = tc.Function.Name
			}
			acc.Function.Arguments += tc.Function.Arguments
		}
	}

	if err := scanner.Err(); err != nil {
		cl.logger.Error("error reading stream", "error", err)
		send(StreamEvent{Err: err})
		return
	}

	final := model.Message{Role: "assistant", Content: content.String()}
	for _, idx := range order {
		final.ToolCalls = append(final.ToolCalls, *toolAcc[idx])
	}

	send(StreamEvent{Done: true, Messages: []model.Message{final}})
}
