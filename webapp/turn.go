package webapp

import (
	"context"
	"errors"
	"html/template"
	"sync"
	"time"

	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

const keepFinished = 2 * time.Minute

var errBusy = errors.New("a reply is still being written in this conversation")

type turn struct {
	id             string
	userId         string
	conversationId string
	userSeq        int
	cancel         context.CancelFunc

	mu      sync.Mutex
	state   turnState
	changed chan struct{}
}

type turnState struct {
	Memories      []string
	Thinking      string
	thinkingStart time.Time
	ThinkingFor   time.Duration
	Trail         []model.Message
	Content       string
	Reply         []model.Message
	Held          *heldView
	Err           string
	Cancelled     bool
	Done          bool
	Title         string
	Finished      bool
}

func (t *turn) update(f func(*turnState)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.state)
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *turn) snapshot() (turnState, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state
	st.Memories = append([]string(nil), st.Memories...)
	st.Trail = append([]model.Message(nil), st.Trail...)
	return st, t.changed
}

type turns struct {
	mu             sync.Mutex
	byId           map[string]*turn
	byConversation map[string]*turn
}

func newTurns() *turns {
	return &turns{byId: map[string]*turn{}, byConversation: map[string]*turn{}}
}

func (ts *turns) start(ctx context.Context, userId, conversationId string, userSeq int) (*turn, context.Context, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.byConversation[conversationId]; ok {
		if st, _ := t.snapshot(); !st.Done {
			return nil, nil, errBusy
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &turn{id: newId(), userId: userId, conversationId: conversationId, userSeq: userSeq, cancel: cancel, changed: make(chan struct{})}
	ts.byId[t.id] = t
	ts.byConversation[conversationId] = t
	return t, ctx, nil
}

func (ts *turns) finish(t *turn) {
	time.AfterFunc(keepFinished, func() {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		delete(ts.byId, t.id)
		if ts.byConversation[t.conversationId] == t {
			delete(ts.byConversation, t.conversationId)
		}
	})
}

func (ts *turns) get(userId, id string) (*turn, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.byId[id]
	return t, ok && t.userId == userId
}

func (ts *turns) running(userId, conversationId string) (*turn, bool) {
	ts.mu.Lock()
	t, ok := ts.byConversation[conversationId]
	ts.mu.Unlock()
	if !ok || t.userId != userId {
		return nil, false
	}
	st, _ := t.snapshot()
	return t, !st.Done
}

func (ts *turns) unfinished(userId, conversationId string) bool {
	ts.mu.Lock()
	t, ok := ts.byConversation[conversationId]
	ts.mu.Unlock()
	if !ok || t.userId != userId {
		return false
	}
	st, _ := t.snapshot()
	return !st.Finished
}

func (s *Server) runTurn(ctx context.Context, t *turn, client llm.ChatClient, conv model.Conversation) {
	defer s.turns.finish(t)
	defer t.update(func(st *turnState) { st.Finished = true })
	saveCtx := context.WithoutCancel(ctx)

	for ev := range s.app.Agent.Run(ctx, client, conv) {
		t.update(func(st *turnState) {
			switch {
			case ev.Err != nil:
				st.Err = ev.Err.Error()
			case ev.Reply != nil:
				st.Reply = ev.Reply
				st.Content = ""
			case ev.Trail != nil:
				st.Content = ""
				st.Trail = append(st.Trail, ev.Trail...)
			case ev.Memories != nil:
				for _, m := range ev.Memories {
					st.Memories = append(st.Memories, m.Content)
				}
			}
			if ev.Reasoning != "" {
				if st.Thinking == "" {
					st.thinkingStart = time.Now()
				}
				st.Thinking += ev.Reasoning
				st.ThinkingFor = time.Since(st.thinkingStart)
			}
			st.Content += ev.Content
		})
		// Each tool round is saved as it happens, so a turn stopped by the user,
		// the tool limit or a restart can be picked up where it was.
		if ev.Trail != nil {
			for _, m := range ev.Trail {
				m.Seq = len(conv.Messages) + 1
				conv.Messages = append(conv.Messages, m)
			}
			if _, err := s.app.History.SaveConversation(saveCtx, conv); err != nil {
				s.logger.Error("Could not save the tool round", "conversation", conv.Id, "error", err)
			}
		}
	}

	st, _ := t.snapshot()
	failed := st.Err != "" || ctx.Err() != nil
	reply := st.Reply
	// A reply that broke off on an error is not saved: it would go back to the
	// model as if it were a finished answer.
	if reply == nil && st.Content != "" && st.Err == "" {
		reply = []model.Message{{Role: "assistant", Content: st.Content}}
	}
	for _, m := range reply {
		m.Seq = len(conv.Messages) + 1
		conv.Messages = append(conv.Messages, m)
	}
	if saved, err := s.app.History.SaveConversation(saveCtx, conv); err != nil {
		s.logger.Error("Could not save the reply", "conversation", conv.Id, "error", err)
		failed = true
		t.update(func(st *turnState) { st.Err = "The reply could not be saved: " + err.Error() })
	} else {
		// The saved copies carry the time the reply was saved at.
		reply = saved.Messages[len(saved.Messages)-len(reply):]
	}
	var held *heldView
	if hasHeld(conv.Messages) {
		if stored, err := s.app.History.GetConversation(saveCtx, conv.Id); err != nil {
			s.logger.Error("Could not load the held calls", "conversation", conv.Id, "error", err)
		} else {
			held = heldCalls(stored)
		}
	}
	t.update(func(st *turnState) {
		st.Reply = reply
		st.Held = held
		st.Content = ""
		st.Cancelled = ctx.Err() != nil && st.Err == ""
		st.Done = true
	})
	if failed {
		return
	}

	if conv.Title == "" && conv.TitleSource != model.TitleSourceUser {
		meta, err := client.Rename(saveCtx, conv.Messages)
		if err == nil {
			conv.Title, conv.TitleSource, conv.Summary = meta.Title, model.TitleSourceLLM, meta.Summary
			_, err = s.app.History.SaveConversation(saveCtx, conv)
		}
		if err != nil {
			s.logger.Error("Could not name the conversation", "conversation", conv.Id, "error", err)
		} else {
			t.update(func(st *turnState) { st.Title = conv.Title })
		}
	}
	if err := s.summarize(saveCtx, client, conv); err != nil {
		s.logger.Error("Could not summarize the conversation", "conversation", conv.Id, "error", err)
	}
}

func (s *Server) summarize(ctx context.Context, client llm.ChatClient, conv model.Conversation) error {
	llmCfg := client.Cfg.Llm
	pending := conv.Unsummarized()
	if llmCfg.ContextBudget == 0 || len(pending) <= llmCfg.KeepRecent ||
		model.EstimateTokens(conv.ContextMessages()) <= llmCfg.ContextBudget {
		return nil
	}
	fold := pending[:len(pending)-llmCfg.KeepRecent]
	previous := ""
	if conv.ContextSummary != nil {
		previous = conv.ContextSummary.Content
	}
	content, err := client.Summarize(ctx, previous, fold)
	if err != nil {
		return err
	}
	return s.app.History.SaveContextSummary(ctx, conv.Id, model.ContextSummary{ThroughSeq: fold[len(fold)-1].Seq, Content: content})
}

type trailLine struct {
	Kind string
	Text string
}

func trailLines(messages []model.Message) []trailLine {
	var lines []trailLine
	for _, m := range messages {
		if m.Role == "tool" {
			lines = append(lines, trailLine{Kind: "result", Text: oneLine(m.Content, 140)})
			continue
		}
		for _, call := range m.ToolCalls {
			lines = append(lines, trailLine{Kind: "call", Text: call.Function.Name + " " + oneLine(call.Function.Arguments, 100)})
		}
	}
	return lines
}

type liveView struct {
	TurnId      string
	Memories    []string
	Thinking    string
	ThinkingFor int
	Thinks      bool
	Steps       []messageView
	HTML        template.HTML
	Messages    []messageView
	Held        *heldView
	Err         string
	Cancelled   bool
	Done        bool
}

func (s *Server) liveView(t *turn, st turnState) liveView {
	v := liveView{
		TurnId:      t.id,
		Memories:    st.Memories,
		Thinking:    st.Thinking,
		ThinkingFor: max(int(st.ThinkingFor.Round(time.Second).Seconds()), 1),
		Thinks:      !st.Done && st.Content == "" && len(st.Reply) == 0,
		Err:         st.Err,
		Cancelled:   st.Cancelled,
		Done:        st.Done,
	}
	v.Steps = messageViews(st.Trail, s.location)
	if st.Done {
		v.Messages = messageViews(st.Reply, s.location)
		v.Held = st.Held
	} else if st.Content != "" {
		v.HTML = renderMarkdown(st.Content)
	}
	return v
}
