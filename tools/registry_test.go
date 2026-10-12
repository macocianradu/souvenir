package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/db/users"
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
	specs := echoRegistry(&seen).Specs(context.Background())
	if len(specs) != 1 || specs[0].Type != "function" || specs[0].Function.Name != "echo" {
		t.Fatalf("%+v", specs)
	}
	var none *Registry
	if none.Specs(context.Background()) != nil {
		t.Fatal("a nil registry offers no tools")
	}
}

func TestForUser_HidesAndRefusesTheToolForOthers(t *testing.T) {
	var seen json.RawMessage
	r := NewRegistry(ForUser("owner-id", echoRegistry(&seen).tools...)...)
	owner := users.WithUser(context.Background(), users.User{Id: "owner-id"})
	other := users.WithUser(context.Background(), users.User{Id: "other-id"})
	if len(r.Specs(owner)) != 1 || len(r.Specs(other)) != 0 {
		t.Fatal("the tool is offered to the wrong users")
	}
	if got := r.Call(other, call("echo", "{}")); !strings.Contains(got.Content, "unknown tool") || seen != nil {
		t.Fatalf("another user ran the tool: %+v", got)
	}
	if got := r.Call(owner, call("echo", "{}")); got.Content != "ok" {
		t.Fatalf("%+v", got)
	}
}
