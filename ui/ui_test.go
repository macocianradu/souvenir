package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
	"git.estatecloud.org/radumaco/souvenir/tools"
	"github.com/charmbracelet/x/ansi"
)

type harness struct {
	t *testing.T
	m uiModel
}

func newHarness(t *testing.T, apiUrl string, registry *tools.Registry) *harness {
	t.Helper()
	cfg := config.Config{
		Api: config.ApiConfig{Url: apiUrl, Timeout: 1000},
		Llm: config.LLMConfig{MaxToolRounds: 3, KeepRecent: 2},
	}
	h := &harness{t: t, m: InitialModel(context.Background(), cfg, history.DbClient{}, nil, registry)}
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	return h
}

func (h *harness) send(msg tea.Msg) tea.Cmd {
	nm, cmd := h.m.Update(msg)
	h.m = nm.(uiModel)
	return cmd
}

func (h *harness) key(k string) tea.Cmd {
	switch k {
	case "enter":
		return h.send(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		return h.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	}
	var cmd tea.Cmd
	for _, r := range k {
		cmd = h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return cmd
}

func (h *harness) submit(text string) tea.Cmd {
	h.m.textarea.SetValue(text)
	return h.key("enter")
}

func (h *harness) reply(content string) {
	h.send(streamEventMessage{event: llm.StreamEvent{Done: true, Messages: []model.Message{{Role: "assistant", Content: content}}}})
}

// pump runs commands the way the tea runtime would. Saves never reach the
// zero DbClient because tests that pump keep a save marked in flight.
func (h *harness) pump(cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 200 {
			h.t.Fatal("runaway command loop")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, spinner.TickMsg, cursor.BlinkMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			queue = append(queue, h.send(msg))
		}
	}
}

func (h *harness) roles() string {
	var r []string
	for _, msg := range h.m.conversation.Messages {
		r = append(r, msg.Role)
	}
	return strings.Join(r, ",")
}

func (h *harness) status() string { return ansi.Strip(h.m.statusMessage) }
func (h *harness) screen() string { return ansi.Strip(h.m.renderMessages()) }

func TestSend_IgnoresBlankInputAndRefusesWhileAReplyStreams(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("   ")
	if len(h.m.conversation.Messages) != 0 {
		t.Fatal("blank input was sent")
	}
	h.submit("first")
	h.submit("second")
	if h.roles() != "user" || !strings.Contains(h.status(), "Wait for the reply") {
		t.Fatalf("%s / %q", h.roles(), h.status())
	}
}

func TestSave_RunsOneAtATimeAndOnlyCopiesIdsBack(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("hi")
	h.reply("hello")
	if !h.m.saving || !h.m.saveQueued {
		t.Fatal("the reply's save should queue behind the first")
	}
	h.send(conversationSavedMessage{conversation: model.Conversation{Id: "c1", Messages: []model.Message{{Id: "m1", Seq: 1}}}})
	if h.m.conversation.Id != "c1" || h.m.conversation.Messages[0].Id != "m1" || h.roles() != "user,assistant" {
		t.Fatal("an older save snapshot replaced the live conversation")
	}
	if !h.m.saving || h.m.saveQueued {
		t.Fatal("the queued save should start")
	}
	h.send(conversationSavedMessage{conversation: model.Conversation{Id: "other"}})
	if h.m.conversation.Id != "c1" {
		t.Fatal("a save of another conversation was applied")
	}
}

func TestStream_FailureOrEscKeepsThePartialAnswer(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("q")
	h.send(streamEventMessage{event: llm.StreamEvent{Content: "part"}})
	h.send(streamEventMessage{event: llm.StreamEvent{Err: errors.New("boom")}})
	if h.m.streaming || h.roles() != "user,assistant" || !strings.Contains(h.status(), "boom") {
		t.Fatalf("%s / %q", h.roles(), h.status())
	}

	h.submit("again")
	ctx := h.m.streamCtx
	h.send(streamEventMessage{event: llm.StreamEvent{Content: "half"}})
	h.key("esc")
	if ctx.Err() == nil {
		t.Fatal("esc did not cancel")
	}
	h.send(streamClosedMessage{})
	last := h.m.conversation.Messages[len(h.m.conversation.Messages)-1]
	if last.Content != "half" || !strings.Contains(h.status(), "Reply cancelled") {
		t.Fatalf("%+v / %q", last, h.status())
	}
}

func TestCommands(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("/nope")
	if len(h.m.conversation.Messages) != 0 || !strings.Contains(h.status(), "Unknown command /nope") {
		t.Fatal("unknown command was sent to the model")
	}
	h.submit("/rename")
	if !strings.Contains(h.status(), "Nothing to rename") {
		t.Fatal("rename ran on an empty conversation")
	}
	h.submit("/rename   Trip to Rome ")
	if h.m.conversation.Title != "Trip to Rome" || h.m.conversation.TitleSource != model.TitleSourceUser {
		t.Fatalf("%+v", h.m.conversation)
	}
	h.submit("/models ")
	if h.m.focus != focusModels {
		t.Fatal("tab-completed command with a trailing space did not run")
	}
}

func TestNew_WaitsForInFlightWorkThenResets(t *testing.T) {
	h := newHarness(t, "", nil)
	h.m.conversation = model.Conversation{Id: "c1", Messages: []model.Message{{Role: "user", Seq: 1}}}
	h.m.saving = true
	h.submit("/new")
	if h.m.conversation.Id != "c1" {
		t.Fatal("switched while a save was in flight")
	}
	h.m.saving = false
	h.submit("/new")
	if h.m.conversation.Id != "" || len(h.m.conversation.Messages) != 0 {
		t.Fatal("not reset")
	}
}

func TestAutoTitle(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("q1")
	h.reply("a1")
	if !h.m.titling {
		t.Fatal("no title requested after the first reply")
	}
	h.send(conversationRenamedMessage{title: "Buses", gen: h.m.convGen, auto: true})
	if h.m.conversation.Title != "Buses" || h.m.conversation.TitleSource != model.TitleSourceLLM {
		t.Fatal("generated title not applied")
	}

	h.m.saving, h.m.saveQueued = false, false
	h.submit("/new")
	h.submit("q")
	h.send(streamEventMessage{event: llm.StreamEvent{Err: errors.New("boom")}})
	if h.m.titling {
		t.Fatal("titled after a failed reply")
	}

	h.submit("q")
	h.reply("a")
	gen := h.m.convGen
	h.submit("/rename Mine")
	h.send(conversationRenamedMessage{title: "Generated", gen: gen, auto: true})
	if h.m.conversation.Title != "Mine" {
		t.Fatal("generated title replaced the user's")
	}
	h.send(conversationRenamedMessage{title: "Stale", gen: gen - 1, auto: true})
	if h.m.conversation.Title != "Mine" {
		t.Fatal("result for an earlier conversation applied")
	}
}

func TestRender_ThinkingSitsFadedAboveTheAnswerUntilTheNextMessage(t *testing.T) {
	h := newHarness(t, "", nil)
	h.submit("q1")
	h.send(streamEventMessage{event: llm.StreamEvent{Reasoning: "pondering"}})
	h.reply("answer")
	screen := h.screen()
	if !strings.Contains(screen, "Thinking: pondering") || strings.Index(screen, "pondering") > strings.Index(screen, "Agent: answer") {
		t.Fatalf("\n%s", screen)
	}
	if !strings.Contains(h.m.renderMessages(), "\x1b[38;5;245m") {
		t.Fatal("thinking is not faded")
	}
	h.submit("q2")
	if strings.Contains(h.screen(), "pondering") {
		t.Fatal("thinking kept after the next message")
	}
}

func TestToolLoop_SendsTheTrailButKeepsOnlyTheFinalReply(t *testing.T) {
	var streamed [][]model.Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"T\",\"summary\":\"S\"}"}}]}`)
			return
		}
		streamed = append(streamed, req.Messages)
		if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" && !strings.Contains(last.Content, "loop") {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Final\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"function\":{\"name\":\"echo\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n", len(streamed))
	}))
	defer srv.Close()
	reply := "pong"
	registry := tools.NewRegistry(tools.Tool{Name: "echo", Run: func(context.Context, json.RawMessage) (string, error) {
		return reply, nil
	}})

	h := newHarness(t, srv.URL, registry)
	h.m.saving = true
	h.pump(h.submit("please echo"))
	if h.roles() != "user,assistant" || h.m.conversation.Messages[1].Content != "Final" || h.m.conversation.Messages[1].Seq != 2 {
		t.Fatalf("kept %s", h.roles())
	}
	wire, _ := json.Marshal(streamed[1])
	if !strings.Contains(string(wire), `"tool_call_id":"call_1"`) {
		t.Fatalf("the model did not see the trail: %s", wire)
	}
	if !strings.Contains(h.screen(), "⚙ echo") || !strings.Contains(h.screen(), "↳ pong") {
		t.Fatalf("\n%s", h.screen())
	}

	h.pump(h.submit("again"))
	wire, _ = json.Marshal(streamed[2])
	if strings.Contains(string(wire), "call_1") {
		t.Fatal("an earlier turn's trail was sent")
	}

	reply = "loop"
	before := len(h.m.conversation.Messages)
	h.pump(h.submit("loop forever"))
	if !strings.Contains(h.status(), "tool call limit") || len(h.m.conversation.Messages) != before+1 {
		t.Fatalf("%q, %d messages", h.status(), len(h.m.conversation.Messages))
	}
}

func TestContextSummary_RequestedOverBudgetAndAppliedForThisConversation(t *testing.T) {
	h := newHarness(t, "", nil)
	h.m.client.Cfg.Llm.ContextBudget = 10
	h.m.conversation.Id = "c1"
	for i := range 3 {
		h.submit(fmt.Sprintf("question %d with enough words to pass the budget", i))
		h.reply("an answer with enough words to pass the budget")
	}
	if !h.m.summarizing {
		t.Fatal("no summary requested")
	}
	summary := &model.ContextSummary{ThroughSeq: 4, Content: "s"}
	h.send(summarizedMessage{summary: summary, gen: h.m.convGen - 1})
	if h.m.conversation.ContextSummary != nil {
		t.Fatal("summary for another conversation applied")
	}
	h.send(summarizedMessage{summary: summary, gen: h.m.convGen})
	if h.m.conversation.ContextSummary != summary {
		t.Fatal("summary not applied")
	}
}

func TestScroll_ReadingBackIsNotUndoneByNewOutput(t *testing.T) {
	h := newHarness(t, "", nil)
	for i := 1; i <= 30; i++ {
		h.m.conversation.Messages = append(h.m.conversation.Messages, model.Message{Role: "user", Content: fmt.Sprint("msg ", i), Seq: i})
	}
	h.m.refreshViewport()
	h.m.viewport.GotoBottom()
	h.send(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	h.send(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if h.m.viewport.AtBottom() {
		t.Fatal("did not scroll")
	}

	h.submit("q")
	if !h.m.viewport.AtBottom() {
		t.Fatal("sending should jump to the bottom")
	}
	h.send(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	offset := h.m.viewport.YOffset()
	h.send(streamEventMessage{event: llm.StreamEvent{Content: "token"}})
	if h.m.viewport.YOffset() != offset {
		t.Fatal("streamed output pulled the view down")
	}
	h.m.viewport.GotoBottom()
	h.reply("done")
	if !h.m.viewport.AtBottom() {
		t.Fatal("lost the bottom when the spinner went away")
	}
}

func TestLayout_CommandListFitsTheContentWidth(t *testing.T) {
	h := newHarness(t, "", nil)
	h.key("/")
	if !h.m.commands.open || len(h.m.commands.view()) == 0 {
		t.Fatal("command list not open")
	}
	if w := lipgloss.Width(h.m.commands.view()); w != h.m.viewport.Width() {
		t.Fatalf("command list %d wide, content %d", w, h.m.viewport.Width())
	}
}

func historyPicker(t *testing.T) picker {
	p := newPicker()
	p.SetSize(100, 30)
	p, _ = p.Update(modelsLoadedMsg{searchable: true, models: []modelItem{{id: "a", name: "A"}, {id: "b", name: "B"}, {id: "c", name: "C"}}})
	return p
}

func pickerKey(p picker, k string) (picker, tea.Cmd) {
	switch k {
	case "enter":
		return p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		return p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	}
	var cmd tea.Cmd
	for _, r := range k {
		p, cmd = p.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return p, cmd
}

func TestHistory_SearchRunsOnEnterAndEscStepsBack(t *testing.T) {
	p := historyPicker(t)
	p, _ = pickerKey(p, "/")
	p, _ = pickerKey(p, "box x")
	if p.confirm != nil || p.query.Value() != "box x" {
		t.Fatal("typing in the search box triggered a shortcut")
	}
	p, cmd := pickerKey(p, "enter")
	if msg, ok := cmd().(pickerSearchMsg); !ok || msg.query != "box x" {
		t.Fatal("enter did not request a search")
	}
	p, _ = p.Update(pickerResultsMsg{query: "older", items: nil})
	if len(p.list.Items()) != 3 {
		t.Fatal("stale results applied")
	}
	p, _ = p.Update(pickerResultsMsg{query: "box x", items: []modelItem{{id: "b", name: "B"}}})
	if len(p.list.Items()) != 1 {
		t.Fatal("results not shown")
	}
	p, _ = pickerKey(p, "esc")
	if len(p.list.Items()) != 3 || p.query.Value() != "" {
		t.Fatal("first esc should clear the search")
	}
	if _, cmd := pickerKey(p, "esc"); cmd == nil {
		t.Fatal("second esc should close")
	} else if _, ok := cmd().(pickerDismissMsg); !ok {
		t.Fatal("second esc should close")
	}
}

func TestHistory_DeleteNeedsConfirmationAndStaysDeleted(t *testing.T) {
	p := historyPicker(t)
	p, _ = pickerKey(p, "x")
	if p.confirm == nil || !strings.Contains(ansi.Strip(p.View()), "cannot be recovered") {
		t.Fatal("no confirmation shown")
	}
	p, cmd := pickerKey(p, "n")
	if p.confirm != nil || cmd != nil {
		t.Fatal("any key other than y should cancel")
	}

	p.query.SetValue("q")
	p, _ = p.Update(pickerResultsMsg{query: "q", items: []modelItem{{id: "b", name: "B"}}})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	p, cmd = pickerKey(p, "y")
	if msg, ok := cmd().(pickerDeleteMsg); !ok || msg.item.id != "b" {
		t.Fatal("y did not request the delete")
	}
	p.remove("b")
	p, _ = pickerKey(p, "esc")
	for _, it := range p.list.Items() {
		if it.(modelItem).id == "b" {
			t.Fatal("deleted conversation came back after clearing the search")
		}
	}

	models := newPicker()
	models.SetSize(100, 30)
	models, _ = models.Update(modelsLoadedMsg{models: []modelItem{{id: "m", name: "m"}}})
	models, _ = pickerKey(models, "x")
	if models.confirm != nil {
		t.Fatal("delete offered outside history")
	}
}
