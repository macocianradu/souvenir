package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
)

type heldArg struct {
	Key   string
	Value string
}

type heldCall struct {
	Id     string
	Name   string
	Args   []heldArg
	Result string
	Failed bool
}

type heldView struct {
	ConversationId string
	Anchor         string
	Calls          []heldCall
	Resolved       bool
	Err            string
	Oob            bool
}

func heldCalls(conv model.Conversation) *heldView {
	held := model.HeldCalls(conv.Messages)
	if len(held) == 0 {
		return nil
	}
	v := &heldView{ConversationId: conv.Id, Anchor: "held-" + held[0].Result.Id}
	for _, h := range held {
		v.Calls = append(v.Calls, heldCall{Id: h.Result.Id, Name: h.Call.Function.Name, Args: callArgs(h.Call.Function.Arguments)})
	}
	return v
}

func (v *heldView) lists(ids []string) bool {
	if len(ids) != len(v.Calls) {
		return false
	}
	for _, call := range v.Calls {
		if !slices.Contains(ids, call.Id) {
			return false
		}
	}
	return true
}

func hasHeld(messages []model.Message) bool {
	for _, m := range messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, model.Held) {
			return true
		}
	}
	return false
}

// callArgs lists a call's arguments in the order the model gave them, strings
// unquoted, or the raw text when they are not a JSON object.
func callArgs(raw string) []heldArg {
	whole := []heldArg{{Value: raw}}
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return whole
	}
	var args []heldArg
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return whole
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return whole
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			text = string(value)
		}
		args = append(args, heldArg{Key: fmt.Sprint(key), Value: text})
	}
	return args
}

func (s *Server) resolveHeld(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	id := r.PathValue("id")
	if _, busy := s.turns.running(u.Id, id); busy {
		fail(w, http.StatusConflict, "Wait for the reply to finish first.")
		return
	}
	conv, err := s.app.History.GetConversation(ctx, id)
	if err != nil {
		fail(w, http.StatusNotFound, "This conversation does not exist any more.")
		return
	}
	pending := heldCalls(conv)
	if pending == nil {
		fail(w, http.StatusConflict, "These changes were already applied or rejected.")
		return
	}
	r.ParseForm()
	if !pending.lists(r.Form["held"]) {
		fail(w, http.StatusConflict, "These changes are no longer waiting: a newer message replaced them.")
		return
	}
	var approved []string
	if r.FormValue("decision") == "apply" {
		approved = r.Form["approve"]
	}
	resolved, err := s.app.Agent.Resolve(ctx, s.app.History, conv, approved)
	if err != nil {
		s.logger.Error("Could not resolve the held calls", "conversation", conv.Id, "error", err)
	}
	s.render(w, http.StatusOK, "held", outcome(pending, resolved, err))
}

func outcome(pending *heldView, resolved []model.Message, err error) *heldView {
	view := &heldView{ConversationId: pending.ConversationId, Anchor: pending.Anchor, Resolved: true}
	if err != nil {
		view.Err = "Not every change could be recorded: " + err.Error()
	}
	for _, call := range pending.Calls {
		for _, m := range resolved {
			if m.Id != call.Id {
				continue
			}
			call.Result = strings.TrimPrefix(m.Content, model.Approved)
			call.Failed = strings.HasPrefix(call.Result, "Error:")
			view.Calls = append(view.Calls, call)
		}
	}
	return view
}

func (s *Server) rejectHeld(ctx context.Context, conv model.Conversation) (model.Conversation, *heldView) {
	pending := heldCalls(conv)
	if pending == nil {
		return conv, nil
	}
	resolved, err := s.app.Agent.Resolve(ctx, s.app.History, conv, nil)
	if err != nil {
		s.logger.Error("Could not reject the held calls", "conversation", conv.Id, "error", err)
	}
	conv.Messages = slices.Clone(conv.Messages)
	for i, m := range conv.Messages {
		for _, r := range resolved {
			if r.Id == m.Id {
				conv.Messages[i].Content = r.Content
			}
		}
	}
	view := outcome(pending, resolved, err)
	view.Oob = true
	return conv, view
}
