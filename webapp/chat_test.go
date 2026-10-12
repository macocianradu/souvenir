package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

// fakeLLM streams a reply with some thinking. A message containing "wait"
// streams "Partial" and then holds the reply open until it is stopped.
type fakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	models []string
}

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"small"},{"id":"large"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"Greeting\",\"summary\":\"S\"}"}}]}`)
			return
		}
		f.mu.Lock()
		f.models = append(f.models, req.Model)
		f.mu.Unlock()
		last := req.Messages[len(req.Messages)-1].Content
		chunk := func(field, text string) {
			data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{field: text}}}})
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		if strings.Contains(last, "wait") {
			chunk("content", "Partial")
			<-r.Context().Done()
			return
		}
		chunk("reasoning_content", "Pondering.")
		chunk("content", "Hello ")
		chunk("content", "**there**")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) lastModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.models) == 0 {
		return ""
	}
	return f.models[len(f.models)-1]
}

func (h *harness) as(name string) *http.Cookie {
	h.t.Helper()
	w := h.signIn(signIn{claims: map[string]any{"sub": "sub-" + name, "preferred_username": name}})
	c := cookie(w, "souvenir-session")
	if c == nil {
		h.t.Fatalf("could not sign in %s: %d %s", name, w.Code, w.Body)
	}
	return c
}

func (h *harness) post(session *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://souvenir.test")
	if session != nil {
		r.AddCookie(session)
	}
	return h.do(r)
}

func (h *harness) get(session *http.Cookie, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if session != nil {
		r.AddCookie(session)
	}
	return h.do(r)
}

var turnIdPattern = regexp.MustCompile(`data-turn="([^"]+)"`)

func turnId(t *testing.T, body string) string {
	m := turnIdPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no turn in %s", body)
	}
	return m[1]
}

// waitFor polls the turn until done says so.
func (h *harness) waitFor(session *http.Cookie, id string, done func(turnState) bool) turnState {
	h.t.Helper()
	u, err := h.server.sessions.User(context.Background(), session.Value)
	if err != nil {
		h.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if t, ok := h.server.turns.get(u.Id, id); ok {
			if st, _ := t.snapshot(); done(st) {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("the turn did not get there in time")
	return turnState{}
}

func TestChat_SendStreamsSavesAndNamesTheConversation(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")

	w := h.post(dana, "/messages", url.Values{"content": {"hi <b>there</b>"}, "model": {"large"}})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, "hi &lt;b&gt;there&lt;/b&gt;") {
		t.Fatalf("the message is not shown escaped: %s", body)
	}
	pushed := w.Header().Get("HX-Push-Url")
	convId := strings.TrimPrefix(pushed, "/c/")
	if convId == "" || !strings.Contains(body, `id="conversation-id" value="`+convId+`" hx-swap-oob="true"`) {
		t.Fatalf("the new conversation is not handed to the page: %q %s", pushed, body)
	}
	if c := cookie(w, "souvenir-model"); c == nil || c.Value != "large" {
		t.Fatal("the chosen model is not remembered")
	}

	events := h.get(dana, "/turns/"+turnId(t, body)+"/events").Body.String()
	for _, want := range []string{"event: done", "<strong>there</strong>", "Thought for", "event: title\ndata: Greeting", "event: sidebar", "event: end"} {
		if !strings.Contains(events, want) {
			t.Errorf("the stream lacks %q:\n%s", want, events)
		}
	}
	if h.llm.lastModel() != "large" {
		t.Errorf("answered with %q", h.llm.lastModel())
	}

	conv, err := h.history.GetConversation(users.WithUser(context.Background(), h.user(dana)), convId)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "Hello **there**" || conv.Title != "Greeting" {
		t.Fatalf("saved %+v", conv)
	}

	page := h.get(dana, pushed).Body.String()
	if !strings.Contains(page, "<strong>there</strong>") || !strings.Contains(page, `<h1 id="chat-title">Greeting</h1>`) {
		t.Fatalf("the conversation page does not show the reply:\n%s", page)
	}
	if !strings.Contains(page, `class="active" aria-current="page">Greeting</a>`) {
		t.Fatal("the sidebar does not mark the open conversation")
	}
}

func (h *harness) user(session *http.Cookie) users.User {
	u, err := h.server.sessions.User(context.Background(), session.Value)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func TestChat_StopKeepsThePartialReplyAndOneReplyAtATime(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")

	w := h.post(dana, "/messages", url.Values{"content": {"please wait"}})
	id := turnId(t, w.Body.String())
	convId := strings.TrimPrefix(w.Header().Get("HX-Push-Url"), "/c/")
	h.waitFor(dana, id, func(st turnState) bool { return st.Content == "Partial" })

	if w := h.post(dana, "/messages", url.Values{"content": {"more"}, "conversation": {convId}}); w.Code != http.StatusConflict {
		t.Fatalf("a second message during a reply answered %d", w.Code)
	}
	if page := h.get(dana, "/c/"+convId).Body.String(); !strings.Contains(page, `data-stream="/turns/`+id+`/events"`) {
		t.Fatal("reopening the conversation does not resume the stream")
	}

	if w := h.post(dana, "/turns/"+id+"/cancel", nil); w.Code != http.StatusNoContent {
		t.Fatalf("stop answered %d", w.Code)
	}
	st := h.waitFor(dana, id, func(st turnState) bool { return st.Finished })
	if !st.Cancelled || st.Title != "" {
		t.Fatalf("%+v", st)
	}
	conv, _ := h.history.GetConversation(users.WithUser(context.Background(), h.user(dana)), convId)
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "Partial" {
		t.Fatalf("saved %+v", conv.Messages)
	}
	if w := h.post(dana, "/messages", url.Values{"content": {"again"}, "conversation": {convId}}); w.Code != http.StatusOK {
		t.Fatalf("after stopping, sending answered %d", w.Code)
	}
}

func TestChat_OtherUsersConversationsAndTurnsAreNotFound(t *testing.T) {
	h := newHarness(t)
	dana, erin := h.as("dana"), h.as("erin")
	w := h.post(dana, "/messages", url.Values{"content": {"please wait"}})
	id := turnId(t, w.Body.String())
	convId := strings.TrimPrefix(w.Header().Get("HX-Push-Url"), "/c/")
	h.waitFor(dana, id, func(st turnState) bool { return st.Content == "Partial" })
	defer h.post(dana, "/turns/"+id+"/cancel", nil)

	if w := h.get(erin, "/c/"+convId); w.Code != http.StatusNotFound {
		t.Errorf("erin opened dana's conversation: %d", w.Code)
	}
	if w := h.get(erin, "/turns/"+id+"/events"); w.Code != http.StatusNotFound {
		t.Errorf("erin streamed dana's reply: %d", w.Code)
	}
	if w := h.post(erin, "/turns/"+id+"/cancel", nil); w.Code != http.StatusNotFound {
		t.Errorf("erin stopped dana's reply: %d", w.Code)
	}
	if w := h.post(erin, "/messages", url.Values{"content": {"hi"}, "conversation": {convId}}); w.Code != http.StatusNotFound {
		t.Errorf("erin wrote to dana's conversation: %d", w.Code)
	}
	if strings.Contains(h.get(erin, "/").Body.String(), convId) {
		t.Error("erin's sidebar lists dana's conversation")
	}
}

func TestChat_RefusesAnonymousAndCrossSiteRequests(t *testing.T) {
	h := newHarness(t)
	if w := h.get(nil, "/c/x"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("anonymous page: %d", w.Code)
	}
	if w := h.post(nil, "/messages", url.Values{"content": {"hi"}}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous send: %d", w.Code)
	}
	dana := h.as("dana")
	r := httptest.NewRequest("POST", "/messages", strings.NewReader("content=hi"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.test")
	r.AddCookie(dana)
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-site send: %d", w.Code)
	}
	if list, _ := h.history.GetConversations(users.WithUser(context.Background(), h.user(dana))); len(list) != 0 {
		t.Error("a refused send saved a conversation")
	}
	if w := h.post(dana, "/messages", url.Values{"content": {"   "}}); w.Code != http.StatusBadRequest {
		t.Errorf("blank send: %d", w.Code)
	}
}

func TestPages_CarryAStrictPolicyAndCachedAssets(t *testing.T) {
	h := newHarness(t)
	w := h.get(h.as("dana"), "/")
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("policy %q", csp)
	}
	src := regexp.MustCompile(`src="(/static/app\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(w.Body.String())
	if src == nil {
		t.Fatal("no versioned script")
	}
	asset := h.get(nil, src[1])
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("%d %q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	if b, _ := io.ReadAll(asset.Body); !strings.Contains(string(b), "EventSource") {
		t.Fatal("wrong asset")
	}
}

func TestMarkdown_DropsHTMLAndScriptLinks(t *testing.T) {
	out := string(renderMarkdown("<script>alert(1)</script>\n\n[x](javascript:alert(1)) **ok** <img src=x onerror=alert(1)>"))
	if strings.Contains(out, "<script") || strings.Contains(out, "javascript:") || strings.Contains(out, "onerror") {
		t.Fatalf("unsafe output: %s", out)
	}
	if !strings.Contains(out, "<strong>ok</strong>") {
		t.Fatalf("markdown not rendered: %s", out)
	}
}

func TestSidebar_GroupsByLastUpdate(t *testing.T) {
	loc := time.FixedZone("x", 3600)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	at := func(days int) time.Time { return now.AddDate(0, 0, -days) }
	view := sidebar([]model.Conversation{
		{Id: "a", Title: "A", UpdatedAt: now.Add(-time.Hour)},
		{Id: "b", Preview: "  first   words ", UpdatedAt: at(1)},
		{Id: "c", UpdatedAt: at(3)},
		{Id: "d", Title: "D", UpdatedAt: at(40)},
	}, "b", now)
	var got []string
	for _, g := range view.Groups {
		for _, it := range g.Items {
			got = append(got, fmt.Sprintf("%s:%s:%v", g.Label, it.Name, it.Active))
		}
	}
	want := "Today:A:false Yesterday:first words:true Previous 7 days:Untitled:false Older:D:false"
	if strings.Join(got, " ") != want {
		t.Fatalf("%s", strings.Join(got, " "))
	}
}
