package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/model"
)

func echoRegistry(seen *json.RawMessage) *Registry {
	return NewRegistry(Tool{
		Name:       "echo",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			*seen = args
			if strings.Contains(string(args), "fail") {
				return "", errors.New("it broke")
			}
			return "ok", nil
		},
	})
}

func call(name, args string) model.ToolCall {
	return model.ToolCall{Id: "c1", Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
}

func TestCall_AnswersWithAToolMessageForTheCall(t *testing.T) {
	var seen json.RawMessage
	got := echoRegistry(&seen).Call(context.Background(), call("echo", `{"x":1}`))
	if got.Role != "tool" || got.ToolCallId != "c1" || got.Content != "ok" || string(seen) != `{"x":1}` {
		t.Fatalf("%+v", got)
	}
}

func TestCall_ReportsProblemsToTheModelInsteadOfFailing(t *testing.T) {
	var seen json.RawMessage
	r := echoRegistry(&seen)
	for name, tc := range map[string]struct{ call model.ToolCall }{
		"unknown tool": {call("nope", `{}`)},
		"invalid JSON": {call("echo", `{`)},
		"tool error":   {call("echo", `{"fail":true}`)},
	} {
		t.Run(name, func(t *testing.T) {
			got := r.Call(context.Background(), tc.call)
			if got.Role != "tool" || !strings.HasPrefix(got.Content, "Error: ") {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCall_EmptyArgumentsBecomeAnEmptyObject(t *testing.T) {
	var seen json.RawMessage
	echoRegistry(&seen).Call(context.Background(), call("echo", ""))
	if string(seen) != "{}" {
		t.Fatal(string(seen))
	}
}

func TestSpecs_DescribeEachToolAsAFunction(t *testing.T) {
	var seen json.RawMessage
	specs := echoRegistry(&seen).Specs()
	if len(specs) != 1 || specs[0].Type != "function" || specs[0].Function.Name != "echo" {
		t.Fatalf("%+v", specs)
	}
	var none *Registry
	if none.Specs() != nil {
		t.Fatal("a nil registry offers no tools")
	}
}
