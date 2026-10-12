package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db/settings"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

func (t Tool) Summary() string {
	first, _, _ := strings.Cut(t.Description, ". ")
	return strings.TrimSuffix(first, ".") + "."
}

type Provider struct {
	Catalog []Tool
	Tools func(ctx context.Context) []Tool
}

type Registry struct {
	logger    slog.Logger
	tools     []Tool
	providers []Provider
}

func NewRegistry(tools ...Tool) *Registry {
	return &Registry{logger: *slog.Default().With("Component", "Tools"), tools: tools}
}

func (r *Registry) Provide(p Provider) *Registry {
	r.providers = append(r.providers, p)
	return r
}

func (r *Registry) Catalog() []Tool {
	if r == nil {
		return nil
	}
	all := append([]Tool(nil), r.tools...)
	for _, p := range r.providers {
		all = append(all, p.Catalog...)
	}
	return all
}

func (r *Registry) Tools(ctx context.Context) []Tool {
	if r == nil {
		return nil
	}
	all := append([]Tool(nil), r.tools...)
	for _, p := range r.providers {
		all = append(all, p.Tools(ctx)...)
	}
	chosen := settings.FromContext(ctx)
	var offered []Tool
	for _, t := range all {
		if chosen.ToolEnabled(t.Name) {
			offered = append(offered, t)
		}
	}
	return offered
}

func (r *Registry) Specs(ctx context.Context) []llm.ToolSpec {
	var specs []llm.ToolSpec
	for _, t := range r.Tools(ctx) {
		specs = append(specs, llm.ToolSpec{
			Type:     "function",
			Function: llm.FunctionSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return specs
}

func (r *Registry) Call(ctx context.Context, call model.ToolCall) model.Message {
	result := model.Message{Role: "tool", ToolCallId: call.Id}
	var tool *Tool
	for _, t := range r.Tools(ctx) {
		if t.Name == call.Function.Name {
			tool = &t
			break
		}
	}
	if tool == nil {
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
