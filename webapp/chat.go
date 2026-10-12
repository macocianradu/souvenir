package webapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"git.estatecloud.org/radumaco/souvenir/agent"
	"git.estatecloud.org/radumaco/souvenir/calendar"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/db/sessions"
	"git.estatecloud.org/radumaco/souvenir/db/settings"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
	"git.estatecloud.org/radumaco/souvenir/tools"
)

type App struct {
	Config   config.Config
	History  *history.DbClient
	Agent    *agent.Agent
	Settings *settings.Store
	Tools    *tools.Registry
	// Calendars is nil when calendars are off.
	Calendars *calendar.Store
}

const (
	modelsTTL    = 10 * time.Minute
	maxMessage   = 100_000
	streamEvery  = 80 * time.Millisecond
	streamIdle   = 15 * time.Second
	modelCookie  = "model"
	modelTimeout = 3 * time.Second
)

func newId() string { return sessions.Token() }

type models struct {
	mu      sync.Mutex
	list    []string
	fetched time.Time
}

func (s *Server) models(ctx context.Context) []string {
	s.modelList.mu.Lock()
	defer s.modelList.mu.Unlock()
	if s.modelList.list != nil && time.Since(s.modelList.fetched) < modelsTTL {
		return s.modelList.list
	}
	ctx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()
	list, err := s.llm.Models(ctx)
	if err != nil {
		s.logger.Warn("Could not list models", "error", err)
		return []string{s.llm.Cfg.Llm.Model}
	}
	slices.Sort(list)
	if !slices.Contains(list, s.llm.Cfg.Llm.Model) {
		list = append([]string{s.llm.Cfg.Llm.Model}, list...)
	}
	s.modelList.list, s.modelList.fetched = list, time.Now()
	return list
}

func (s *Server) chosenModel(r *http.Request, available []string) string {
	if m := r.FormValue("model"); m != "" && slices.Contains(available, m) {
		return m
	}
	if c, err := r.Cookie(s.cookieName(modelCookie)); err == nil && slices.Contains(available, c.Value) {
		return c.Value
	}
	return s.llm.Cfg.Llm.Model
}

type chatPage struct {
	User         users.User
	Initials     string
	Conversation model.Conversation
	Heading      headingView
	Messages     []messageView
	Live         *liveView
	Sidebar      sidebarView
	Models       []string
	Model        string
	CanThink     bool
	Think        bool
}

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request, conv model.Conversation) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	list, err := s.app.History.GetConversations(ctx)
	if err != nil {
		s.logger.Error("Could not list conversations", "error", err)
	}
	available := s.models(ctx)
	p := chatPage{
		User:         u,
		Initials:     initials(u),
		Conversation: conv,
		Heading:      heading(conv),
		Messages:     messageViews(conv.Messages),
		Sidebar:      sidebar(list, conv.Id, time.Now().In(s.location)),
		Models:       available,
		Model:        s.chosenModel(r, available),
		CanThink:     s.llm.Cfg.Llm.Thinking != nil,
		Think:        s.llm.Cfg.Llm.Thinking != nil && *s.llm.Cfg.Llm.Thinking,
	}
	if t, ok := s.turns.running(u.Id, conv.Id); ok && conv.Id != "" {
		st, _ := t.snapshot()
		live := newLiveView(t, st)
		p.Live = &live
	}
	s.render(w, http.StatusOK, "chat", p)
}

func (s *Server) newChat(w http.ResponseWriter, r *http.Request) {
	s.chatPage(w, r, model.Conversation{})
}

func (s *Server) conversation(w http.ResponseWriter, r *http.Request) {
	conv, err := s.app.History.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.page(w, http.StatusNotFound, page{Message: "This conversation does not exist.", Home: true})
		return
	}
	s.chatPage(w, r, conv)
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, message)
}

type sentView struct {
	User    messageView
	Live    liveView
	NewId   string
	Heading headingView
	Sidebar sidebarView
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxImageBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		fail(w, http.StatusRequestEntityTooLarge, "That message is too large to send.")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	images, err := formImages(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	content := strings.TrimSpace(r.FormValue("content"))
	switch {
	case content == "" && len(images) == 0:
		fail(w, http.StatusBadRequest, "Write a message first.")
		return
	case len(content) > maxMessage:
		fail(w, http.StatusRequestEntityTooLarge, "That message is too long.")
		return
	}

	var conv model.Conversation
	if id := r.FormValue("conversation"); id != "" {
		if _, busy := s.turns.running(u.Id, id); busy {
			fail(w, http.StatusConflict, "Wait for the reply to finish before sending another message.")
			return
		}
		var err error
		if conv, err = s.app.History.GetConversation(ctx, id); err != nil {
			fail(w, http.StatusNotFound, "This conversation does not exist any more.")
			return
		}
	}
	isNew := conv.Id == ""
	conv.Messages = append(conv.Messages, model.Message{Role: "user", Content: content, Images: images, Seq: len(conv.Messages) + 1})
	saved, err := s.app.History.SaveConversation(ctx, conv)
	if err != nil {
		s.logger.Error("Could not save the message", "error", err)
		fail(w, http.StatusInternalServerError, "Could not save the message.")
		return
	}
	conv = saved

	client := s.llm
	available := s.models(ctx)
	client.Cfg.Llm.Model = s.chosenModel(r, available)
	if client.Cfg.Llm.Thinking != nil {
		think := r.FormValue("think") != ""
		client.Cfg.Llm.Thinking = &think
	}
	t, turnCtx, err := s.turns.start(users.WithUser(s.base, u), u.Id, conv.Id)
	if err != nil {
		fail(w, http.StatusConflict, "Wait for the reply to finish before sending another message.")
		return
	}
	go s.runTurn(turnCtx, t, client, conv)
	s.setCookie(w, modelCookie, client.Cfg.Llm.Model, 365*24*time.Hour)

	view := sentView{User: messageViews(conv.Messages[len(conv.Messages)-1:])[0]}
	st, _ := t.snapshot()
	view.Live = newLiveView(t, st)
	if list, err := s.app.History.GetConversations(ctx); err == nil {
		view.Sidebar = sidebar(list, conv.Id, time.Now().In(s.location))
	}
	if isNew {
		view.NewId = conv.Id
		view.Heading = headingView{Id: conv.Id, Oob: true}
		w.Header().Set("HX-Push-Url", "/c/"+conv.Id)
	}
	s.render(w, http.StatusOK, "sent", view)
}

const maxTitle = 200

// headingView is the open conversation's title above the chat.
type headingView struct {
	Id    string
	Title string
	Oob   bool
}

func heading(conv model.Conversation) headingView {
	return headingView{Id: conv.Id, Title: conv.Title}
}

type renamedView struct {
	Sidebar sidebarView
	Heading *headingView
}

// rename sets a title the user chose; generated titles never replace it. It
// answers with the sidebar, and with the heading when the renamed
// conversation is the one open.
func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	id := r.PathValue("id")
	title := strings.Join(strings.Fields(r.FormValue("title")), " ")
	switch {
	case title == "":
		fail(w, http.StatusBadRequest, "A title cannot be empty.")
		return
	case len([]rune(title)) > maxTitle:
		fail(w, http.StatusBadRequest, fmt.Sprintf("A title can be at most %d characters.", maxTitle))
		return
	case s.turns.unfinished(u.Id, id):
		fail(w, http.StatusConflict, "Wait for the reply to finish before renaming.")
		return
	}
	if err := s.app.History.SetTitle(ctx, id, title, model.TitleSourceUser); err != nil {
		fail(w, http.StatusNotFound, "This conversation does not exist any more.")
		return
	}
	active := r.FormValue("active")
	list, err := s.app.History.GetConversations(ctx)
	if err != nil {
		s.logger.Error("Could not list conversations", "error", err)
	}
	view := renamedView{Sidebar: sidebar(list, active, time.Now().In(s.location))}
	if active == id {
		view.Heading = &headingView{Id: id, Title: title, Oob: true}
	}
	s.render(w, http.StatusOK, "renamed", view)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	u, _ := users.FromContext(r.Context())
	t, ok := s.turns.get(u.Id, r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "That reply has already finished.")
		return
	}
	t.cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	t, ok := s.turns.get(u.Id, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event, data string) {
		fmt.Fprintf(w, "event: %s\n", event)
		for line := range strings.SplitSeq(data, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		flusher.Flush()
	}
	sentDone, sentTitle := false, false
	for {
		st, changed := t.snapshot()
		switch {
		case st.Done && !sentDone:
			send("done", s.renderString("live", newLiveView(t, st)))
			sentDone = true
		case !st.Done:
			send("update", s.renderString("live-body", newLiveView(t, st)))
		}
		if st.Title != "" && !sentTitle {
			send("title", st.Title)
			if list, err := s.app.History.GetConversations(ctx); err == nil {
				send("sidebar", s.renderString("sidebar", sidebar(list, t.conversationId, time.Now().In(s.location))))
			}
			sentTitle = true
		}
		if st.Finished {
			send("end", "")
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		case <-time.After(streamIdle):
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
			continue
		}
		select {
		case <-time.After(streamEvery):
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		s.logger.Error("Could not render", "template", name, "error", err)
		http.Error(w, "Could not render the page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

func (s *Server) renderString(name string, data any) string {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		s.logger.Error("Could not render", "template", name, "error", err)
		return ""
	}
	return b.String()
}
