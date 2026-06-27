package llm

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"

	config "git.estatecloud.org/radumaco/souvenir/config"
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
}

type StreamChoice struct {
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type StreamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
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

func (cl ChatClient) QueryStream(messages []model.Message) (<-chan StreamEvent, error) {
	requestBody := ChatRequest{
		Model:    cl.Cfg.Llm.Model,
		Messages: messages,
		Tools:    config.AvailableTools(),
		Stream:   true,
		Kwargs:   Kwargs{Thinking: false},
	}
	return cl.CallStream(requestBody)
}

func (cl ChatClient) CallStream(requestBody ChatRequest) (<-chan StreamEvent, error) {
	resp, err := cl.CallApi(requestBody)
	if err != nil {
		cl.logger.Error("There was an error during the API call", "error", err)
		return nil, err
	}
	events := make(chan StreamEvent)
	go cl.readStream(resp, events)
	return events, nil
}

func (cl ChatClient) readStream(resp *http.Response, events chan<- StreamEvent) {
	cl.logger.Debug("Started readStrea")
	defer resp.Body.Close()
	defer close(events)

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
			events <- StreamEvent{Err: err}
			return
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.ReasoningContent != "" {
			cl.logger.Debug("Stream got reasoning content", "content", delta.ReasoningContent)
			events <- StreamEvent{Reasoning: delta.ReasoningContent}
		}
		if delta.Content != "" {
			cl.logger.Debug("Stream got content", "content", delta.Content)
			content.WriteString(delta.Content)
			events <- StreamEvent{Content: delta.Content}
		}

		for _, tc := range delta.ToolCalls {
			acc, ok := toolAcc[tc.Index]
			if !ok {
				acc = &model.ToolCall{}
				toolAcc[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.Id != "" {
				acc.Id = tc.Id
			}
			if tc.Function.Name != "" {
				acc.FunctionName = tc.Function.Name
			}
			acc.FunctionArguments += tc.Function.Arguments
		}
	}

	if err := scanner.Err(); err != nil {
		cl.logger.Error("error reading stream", "error", err)
		events <- StreamEvent{Err: err}
		return
	}

	final := model.Message{Role: "assistant", Content: content.String()}
	for _, idx := range order {
		final.ToolCalls = append(final.ToolCalls, *toolAcc[idx])
	}

	events <- StreamEvent{Done: true, Messages: []model.Message{final}}
}
