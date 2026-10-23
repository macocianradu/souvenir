package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
)

// fakeLLM streams a reply with some thinking. A message containing "wait"
// streams "Partial" and then holds the reply open until it is stopped.
type fakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	models []string
	// streamed holds the body of each streamed request.
	streamed []string
}

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"small"},{"id":"large"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Content is a string, or a list of parts when images are attached.
		var req struct {
			Model    string
			Stream   bool
			Messages []struct{ Content json.RawMessage }
		}
		json.Unmarshal(body, &req)
		if !req.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"Greeting\",\"summary\":\"S\"}"}}]}`)
			return
		}
		f.mu.Lock()
		f.models = append(f.models, req.Model)
		f.streamed = append(f.streamed, string(body))
		f.mu.Unlock()
		last := string(req.Messages[len(req.Messages)-1].Content)
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
	if !strings.Contains(page, "<strong>there</strong>") || !strings.Contains(page, `<h1 id="chat-title" title="Greeting">Greeting</h1>`) {
		t.Fatalf("the conversation page does not show the reply:\n%s", page)
	}
	if !strings.Contains(page, `<li class="active">`) || !strings.Contains(page, `title="Greeting" aria-current="page">Greeting</a>`) {
		t.Fatal("the sidebar does not mark the open conversation")
	}
}

func (f *fakeLLM) lastStreamed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streamed) == 0 {
		return ""
	}
	return f.streamed[len(f.streamed)-1]
}

func TestChat_ReasoningEffortIsSentAndRemembered(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")

	w := h.post(dana, "/messages", url.Values{"content": {"hi"}, "effort": {"high"}})
	h.get(dana, "/turns/"+turnId(t, w.Body.String())+"/events")
	if got := h.llm.lastStreamed(); !strings.Contains(got, `"reasoning_effort":"high"`) {
		t.Fatalf("the effort is not sent: %s", got)
	}
	c := cookie(w, "souvenir-effort")
	if c == nil || c.Value != "high" {
		t.Fatal("the chosen effort is not remembered")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(dana)
	r.AddCookie(c)
	if page := h.do(r).Body.String(); !strings.Contains(page, "<option selected>high</option>") {
		t.Fatalf("the composer does not show the remembered effort:\n%s", page)
	}

	w = h.post(dana, "/messages", url.Values{"content": {"hi"}, "effort": {""}})
	h.get(dana, "/turns/"+turnId(t, w.Body.String())+"/events")
	if got := h.llm.lastStreamed(); strings.Contains(got, "reasoning_effort") {
		t.Fatalf("default must leave the effort to the model: %s", got)
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

func TestManifest_MakesTheSiteInstallable(t *testing.T) {
	h := newHarness(t)
	w := h.get(nil, "/manifest.webmanifest")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var m struct {
		StartUrl string `json:"start_url"`
		Display  string
		Icons    []struct{ Src, Sizes, Purpose string }
	}
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.StartUrl != "/" || m.Display != "standalone" {
		t.Fatalf("%+v", m)
	}
	have := map[string]bool{}
	for _, icon := range m.Icons {
		have[icon.Sizes+" "+icon.Purpose] = true
		if asset := h.get(nil, icon.Src); asset.Code != http.StatusOK || asset.Header().Get("Content-Type") != "image/png" {
			t.Errorf("icon %s: %d %q", icon.Src, asset.Code, asset.Header().Get("Content-Type"))
		}
	}
	for _, want := range []string{"192x192 any", "512x512 any", "512x512 maskable"} {
		if !have[want] {
			t.Errorf("no %s icon", want)
		}
	}

	page := h.get(nil, "/").Body.String()
	for _, want := range []string{`rel="manifest" href="/manifest.webmanifest"`, `rel="apple-touch-icon" href="/static/apple-touch-icon.png?v=`, `name="apple-mobile-web-app-title" content="Souvenir"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "manifest-src 'self'") {
		t.Error("the policy blocks the manifest")
	}
	if a := h.get(nil, "/static/apple-touch-icon.png"); a.Code != http.StatusOK {
		t.Errorf("apple touch icon: %d", a.Code)
	}
}

// png is a 1x1 PNG.
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func (h *harness) postImages(session *http.Cookie, content string, images map[string][]byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("content", content)
	for name, data := range images {
		part, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="images"; filename="` + name + `"`},
			"Content-Type":        {"image/png"},
		})
		part.Write(data)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/messages", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Origin", "http://souvenir.test")
	r.AddCookie(session)
	return h.do(r)
}

func TestChat_PastedImagesReachTheModelAndAreSaved(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")

	w := h.postImages(dana, "", map[string][]byte{"shot.png": png})
	if w.Code != http.StatusOK {
		t.Fatalf("an image-only message answered %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `<img src="data:image/png;base64,`) {
		t.Fatal("the sent message does not show the image")
	}
	h.get(dana, "/turns/"+turnId(t, w.Body.String())+"/events")
	h.llm.mu.Lock()
	sent := h.llm.streamed[len(h.llm.streamed)-1]
	h.llm.mu.Unlock()
	if !strings.Contains(sent, `"image_url":{"url":"data:image/png;base64,`) {
		t.Fatalf("the model did not get the image: %s", sent)
	}
	convId := strings.TrimPrefix(w.Header().Get("HX-Push-Url"), "/c/")
	conv, err := h.history.GetConversation(users.WithUser(context.Background(), h.user(dana)), convId)
	if err != nil || len(conv.Messages) == 0 || len(conv.Messages[0].Images) != 1 || !bytes.Equal(conv.Messages[0].Images[0].Data, png) {
		t.Fatalf("the image was not saved: %v", err)
	}
	if !strings.Contains(h.get(dana, "/c/"+convId).Body.String(), `<img src="data:image/png;base64,`) {
		t.Fatal("reopening the conversation does not show the image")
	}

	if w := h.postImages(dana, "look", map[string][]byte{"fake.png": []byte("<script>alert(1)</script>")}); w.Code != http.StatusBadRequest {
		t.Errorf("a non-image claiming to be a PNG answered %d", w.Code)
	}
	many := map[string][]byte{}
	for i := range maxImages + 1 {
		many[fmt.Sprint(i, ".png")] = png
	}
	if w := h.postImages(dana, "look", many); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at most") {
		t.Errorf("too many images answered %d", w.Code)
	}
}

func TestRename_SetsTheUsersTitleInPlace(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	ctx := users.WithUser(context.Background(), h.user(dana))
	older, _ := h.history.SaveConversation(ctx, model.Conversation{Title: "Older", Messages: []model.Message{{Role: "user", Content: "a", Seq: 1}}})
	newer, _ := h.history.SaveConversation(ctx, model.Conversation{Title: "Newer", Messages: []model.Message{{Role: "user", Content: "b", Seq: 1}}})
	long := strings.Repeat("A rather long title ", 8)

	w := h.post(dana, "/c/"+older.Id+"/title", url.Values{"title": {"  " + long + "  "}, "active": {older.Id}})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	clean := strings.TrimSpace(long)
	if !strings.Contains(body, `title="`+clean+`"`) {
		t.Error("the sidebar does not carry the full title as a tooltip")
	}
	if !strings.Contains(body, `id="chat-heading" hx-swap-oob="true"`) || !strings.Contains(body, "<title>"+clean+" · Souvenir</title>") {
		t.Errorf("the open conversation's heading is not updated:\n%s", body)
	}
	if strings.Index(body, "Newer") > strings.Index(body, clean) {
		t.Error("renaming moved the conversation up the list")
	}
	conv, _ := h.history.GetConversation(ctx, older.Id)
	if conv.Title != clean || conv.TitleSource != model.TitleSourceUser {
		t.Fatalf("saved %q from %q", conv.Title, conv.TitleSource)
	}

	w = h.post(dana, "/c/"+newer.Id+"/title", url.Values{"title": {"Renamed elsewhere"}, "active": {older.Id}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "chat-heading") {
		t.Errorf("renaming another conversation touched the open one's heading: %d", w.Code)
	}

	for name, title := range map[string]string{"empty": "   ", "too long": strings.Repeat("x", maxTitle+1)} {
		if w := h.post(dana, "/c/"+older.Id+"/title", url.Values{"title": {title}}); w.Code != http.StatusBadRequest {
			t.Errorf("%s title answered %d", name, w.Code)
		}
	}
	if w := h.post(h.as("erin"), "/c/"+older.Id+"/title", url.Values{"title": {"Mine"}}); w.Code != http.StatusNotFound {
		t.Errorf("erin renamed dana's conversation: %d", w.Code)
	}

	sent := h.post(dana, "/messages", url.Values{"content": {"please wait"}, "conversation": {older.Id}})
	id := turnId(t, sent.Body.String())
	h.waitFor(dana, id, func(st turnState) bool { return st.Content == "Partial" })
	if w := h.post(dana, "/c/"+older.Id+"/title", url.Values{"title": {"During"}}); w.Code != http.StatusConflict {
		t.Errorf("renaming during a reply answered %d", w.Code)
	}
	h.post(dana, "/turns/"+id+"/cancel", nil)
}

func TestStamp_ShortensWithRecency(t *testing.T) {
	loc := time.FixedZone("Brussels", 2*3600)
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, loc) // a Wednesday
	for at, want := range map[time.Time]string{
		time.Date(2026, 10, 14, 0, 5, 0, 0, loc):   "00:05",
		time.Date(2026, 10, 13, 23, 59, 0, 0, loc): "Yesterday 23:59",
		time.Date(2026, 10, 8, 14, 0, 0, 0, loc):   "Thu 14:00",
		time.Date(2026, 10, 7, 14, 0, 0, 0, loc):   "7 Oct 14:00",
		time.Date(2025, 12, 31, 18, 0, 0, 0, loc):  "31 Dec 2025 18:00",
		// Saved in UTC, shown in the server's zone.
		time.Date(2026, 10, 13, 22, 30, 0, 0, time.UTC): "00:30",
	} {
		s := newStamp(at, now)
		if s.Short != want || s.ISO != at.UTC().Format(time.RFC3339) {
			t.Errorf("%s: %+v, want %q", at, s, want)
		}
	}
	if newStamp(time.Time{}, now) != (stamp{}) {
		t.Error("an unsaved message has a time")
	}
}

func TestChat_MessagesShowWhenTheyWereSent(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	w := h.post(dana, "/messages", url.Values{"content": {"hi"}})
	stamped := regexp.MustCompile(`<time class="stamp" datetime="[0-9T:\-]+Z" title="[^"]+">[0-9]{2}:[0-9]{2}</time>`)
	if !stamped.MatchString(w.Body.String()) {
		t.Fatalf("the sent message has no time:\n%s", w.Body)
	}
	events := h.get(dana, "/turns/"+turnId(t, w.Body.String())+"/events").Body.String()
	done := events[strings.Index(events, "event: done"):]
	if !stamped.MatchString(done) {
		t.Fatalf("the reply has no time:\n%s", done)
	}
	page := h.get(dana, w.Header().Get("HX-Push-Url")).Body.String()
	if n := len(stamped.FindAllString(page, -1)); n != 2 {
		t.Fatalf("the reopened conversation shows %d times, want 2", n)
	}
}

func TestDelete_RemovesTheConversationButNotWhileReplying(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	ctx := users.WithUser(context.Background(), h.user(dana))
	open, _ := h.history.SaveConversation(ctx, model.Conversation{Title: "Open one", Messages: []model.Message{{Role: "user", Content: "a", Seq: 1}}})
	other, _ := h.history.SaveConversation(ctx, model.Conversation{Title: "Other one", Messages: []model.Message{{Role: "user", Content: "b", Seq: 1}}})

	if page := h.get(dana, "/c/"+open.Id).Body.String(); !strings.Contains(page, `hx-post="/c/`+open.Id+`/delete"`) {
		t.Fatal("the page offers no delete button")
	}
	if w := h.post(h.as("erin"), "/c/"+open.Id+"/delete", nil); w.Code != http.StatusNotFound {
		t.Errorf("erin deleted dana's conversation: %d", w.Code)
	}

	w := h.post(dana, "/c/"+other.Id+"/delete", url.Values{"conversation": {open.Id}})
	if w.Code != http.StatusOK || w.Header().Get("HX-Location") != "" {
		t.Fatalf("deleting another conversation: %d %q", w.Code, w.Header().Get("HX-Location"))
	}
	if body := w.Body.String(); strings.Contains(body, "Other one") || !strings.Contains(body, "Open one") {
		t.Errorf("the sidebar is not updated:\n%s", body)
	}
	if _, err := h.history.GetConversation(ctx, other.Id); err == nil {
		t.Error("the conversation is still saved")
	}

	sent := h.post(dana, "/messages", url.Values{"content": {"please wait"}, "conversation": {open.Id}})
	id := turnId(t, sent.Body.String())
	h.waitFor(dana, id, func(st turnState) bool { return st.Content == "Partial" })
	if w := h.post(dana, "/c/"+open.Id+"/delete", url.Values{"conversation": {open.Id}}); w.Code != http.StatusConflict {
		t.Errorf("deleting during a reply answered %d", w.Code)
	}
	h.post(dana, "/turns/"+id+"/cancel", nil)
	h.waitFor(dana, id, func(st turnState) bool { return st.Finished })

	w = h.post(dana, "/c/"+open.Id+"/delete", url.Values{"conversation": {open.Id}})
	if w.Code != http.StatusOK || w.Header().Get("HX-Location") != "/" {
		t.Errorf("deleting the open conversation did not leave it: %d %q", w.Code, w.Header().Get("HX-Location"))
	}
	if w := h.get(dana, "/c/"+open.Id); w.Code != http.StatusNotFound {
		t.Errorf("the deleted conversation still opens: %d", w.Code)
	}
}

func TestHeld_ReviewShowsEachCallAndAppliesOnlyTheChosenOnes(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	ctx := users.WithUser(context.Background(), h.user(dana))
	create := func(id, summary string) model.ToolCall {
		args, _ := json.Marshal(map[string]string{"calendar": "assistant", "summary": summary, "start": "2026-10-23T12:00-05:00"})
		return model.ToolCall{Id: id, Type: "function", Function: model.FunctionCall{Name: "calendar_create", Arguments: string(args)}}
	}
	held := model.Held + ", because untrusted content was read earlier in this turn."
	conv, err := h.history.SaveConversation(ctx, model.Conversation{Messages: []model.Message{
		{Role: "user", Content: "add the matches", Seq: 1},
		{Role: "assistant", ToolCalls: []model.ToolCall{create("c1", "Gen.G vs IG"), create("c2", "HLE vs TES")}, Seq: 2},
		{Role: "tool", ToolCallId: "c1", Content: held, Seq: 3},
		{Role: "tool", ToolCallId: "c2", Content: held, Seq: 4},
		{Role: "assistant", Content: "I'd add two matches.", Seq: 5},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := h.history.GetConversation(ctx, conv.Id)
	first, second := stored.Messages[2].Id, stored.Messages[3].Id

	page := h.get(dana, "/c/"+conv.Id).Body.String()
	for _, want := range []string{`hx-post="/c/` + conv.Id + `/held"`, "calendar_create", "<dt>summary</dt><dd>Gen.G vs IG</dd>",
		"<dd>2026-10-23T12:00-05:00</dd>", `value="` + first + `"`, `value="` + second + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the review lacks %q:\n%s", want, page)
		}
	}
	if w := h.post(h.as("erin"), "/c/"+conv.Id+"/held", url.Values{"decision": {"apply"}, "held": {first, second}, "approve": {first}}); w.Code != http.StatusNotFound {
		t.Fatalf("erin resolved dana's calls: %d", w.Code)
	}

	w := h.post(dana, "/c/"+conv.Id+"/held", url.Values{"decision": {"apply"}, "held": {first, second}, "approve": {first}})
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, "Created:") || !strings.Contains(body, model.Rejected) {
		t.Fatalf("%d\n%s", w.Code, body)
	}
	events, _ := filepath.Glob(filepath.Join(h.calendarRoot, "dana", "assistant", "*.ics"))
	if len(events) != 1 {
		t.Fatalf("%d events written, want 1", len(events))
	}
	if w := h.post(dana, "/c/"+conv.Id+"/held", url.Values{"decision": {"apply"}, "held": {first, second}, "approve": {first, second}}); w.Code != http.StatusConflict {
		t.Fatalf("resolving again answered %d", w.Code)
	}
	if page := h.get(dana, "/c/"+conv.Id).Body.String(); strings.Contains(page, "/held\"") {
		t.Fatal("the review is still offered after resolving")
	}
	after, _ := h.history.GetConversation(ctx, conv.Id)
	if !strings.HasPrefix(after.Messages[2].Content, model.Approved+"Created:") || after.Messages[3].Content != model.Rejected {
		t.Fatalf("stored results %q, %q", after.Messages[2].Content, after.Messages[3].Content)
	}
}

func TestHeld_AFormFromBeforeANewerMessageDecidesNothing(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	ctx := users.WithUser(context.Background(), h.user(dana))
	create := func(id string) model.ToolCall {
		return model.ToolCall{Id: id, Type: "function", Function: model.FunctionCall{Name: "calendar_create", Arguments: "{}"}}
	}
	conv, err := h.history.SaveConversation(ctx, model.Conversation{Messages: []model.Message{
		{Role: "user", Content: "add the matches", Seq: 1},
		{Role: "assistant", ToolCalls: []model.ToolCall{create("old")}, Seq: 2},
		{Role: "tool", ToolCallId: "old", Content: model.Held, Seq: 3},
		{Role: "user", Content: "and remember this", Seq: 4},
		{Role: "assistant", ToolCalls: []model.ToolCall{create("new")}, Seq: 5},
		{Role: "tool", ToolCallId: "new", Content: model.Held, Seq: 6},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := h.history.GetConversation(ctx, conv.Id)
	old := stored.Messages[2].Id
	w := h.post(dana, "/c/"+conv.Id+"/held", url.Values{"decision": {"apply"}, "held": {old}, "approve": {old}})
	if w.Code != http.StatusConflict {
		t.Fatalf("the stale form answered %d\n%s", w.Code, w.Body.String())
	}
	after, _ := h.history.GetConversation(ctx, conv.Id)
	if after.Messages[5].Content != model.Held {
		t.Fatalf("the current call was decided by the stale form: %q", after.Messages[5].Content)
	}
}

func TestHeld_SendingANewMessageRejectsTheWaitingChanges(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	ctx := users.WithUser(context.Background(), h.user(dana))
	call := model.ToolCall{Id: "c1", Type: "function", Function: model.FunctionCall{Name: "calendar_create", Arguments: "{}"}}
	conv, err := h.history.SaveConversation(ctx, model.Conversation{Messages: []model.Message{
		{Role: "user", Content: "add the match", Seq: 1},
		{Role: "assistant", ToolCalls: []model.ToolCall{call}, Seq: 2},
		{Role: "tool", ToolCallId: "c1", Content: model.Held, Seq: 3},
		{Role: "assistant", Content: "I'd add it.", Seq: 4},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := h.history.GetConversation(ctx, conv.Id)
	held := stored.Messages[2].Id

	w := h.post(dana, "/messages", url.Values{"content": {"never mind"}, "conversation": {conv.Id}})
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `id="held-`+held+`" class="held resolved" hx-swap-oob="outerHTML"`) ||
		!strings.Contains(body, model.Rejected) {
		t.Fatalf("%d\n%s", w.Code, body)
	}
	h.waitFor(dana, turnId(t, body), func(st turnState) bool { return st.Finished })
	after, _ := h.history.GetConversation(ctx, conv.Id)
	if after.Messages[2].Content != model.Rejected {
		t.Fatalf("stored result %q", after.Messages[2].Content)
	}
}

func TestCallArgs_KeepTheModelsOrderAndUnquoteStrings(t *testing.T) {
	got := callArgs(`{"summary":"Final","start":"2026-11-14T20:00","exceptions":["2026-11-21"],"n":3}`)
	want := []heldArg{{"summary", "Final"}, {"start", "2026-11-14T20:00"}, {"exceptions", `["2026-11-21"]`}, {"n", "3"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%v", got)
	}
	if got := callArgs(`not json`); len(got) != 1 || got[0].Key != "" || got[0].Value != "not json" {
		t.Fatalf("%v", got)
	}
}
