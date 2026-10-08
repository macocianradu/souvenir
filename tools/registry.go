package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

type Registry struct {
	logger slog.Logger
	tools  []Tool
	byName map[string]Tool
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{
		logger: *slog.Default().With("Component", "Tools"),
		byName: map[string]Tool{},
	}
	for _, t := range tools {
		r.tools = append(r.tools, t)
		r.byName[t.Name] = t
	}
	return r
}

func (r *Registry) Specs() []llm.ToolSpec {
	if r == nil {
		return nil
	}
	specs := make([]llm.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, llm.ToolSpec{
			Type:     "function",
			Function: llm.FunctionSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return specs
}

func (r *Registry) Call(ctx context.Context, call model.ToolCall) model.Message {
	result := model.Message{Role: "tool", ToolCallId: call.Id}
	tool, ok := r.byName[call.Function.Name]
	if !ok {
		result.Content = fmt.Sprintf("Error: unknown tool %q", call.Function.Name)
		return result
	}
	args := json.RawMessage(call.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		result.Content = "Error: arguments are not valid JSON"
		return result
	}
	r.logger.Debug("Running tool", "name", tool.Name, "args", string(args))
	out, err := tool.Run(ctx, args)
	if err != nil {
		r.logger.Warn("Tool failed", "name", tool.Name, "error", err)
		result.Content = "Error: " + err.Error()
		return result
	}
	result.Content = out
	return result
}
