package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func sse(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
			w.(http.Flusher).Flush()
		}
	}
}

func newClient(t *testing.T, h http.Handler) ChatClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return *NewLLMClient(config.Config{Api: config.ApiConfig{Url: srv.URL, Timeout: 1000}})
}

type collected struct {
	content, reasoning string
	last               StreamEvent
}

func collect(t *testing.T, ch <-chan StreamEvent) collected {
	t.Helper()
	var c collected
	for ev := range ch {
		c.content += ev.Content
		c.reasoning += ev.Reasoning
		c.last = ev
	}
	return c
}

func TestStream_SeparatesContentFromReasoningInEitherFieldName(t *testing.T) {
	cl := newClient(t, sse(
		`{"choices":[{"delta":{"reasoning_content":"llama "}}]}`,
		`{"choices":[{"delta":{"reasoning":"openrouter"}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`[DONE]`))
	ch, err := cl.QueryStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if got.content != "Hello" || got.reasoning != "llama openrouter" || !got.last.Done {
		t.Fatalf("%+v", got)
	}
	if got.last.Messages[0].Content != "Hello" || got.last.Messages[0].Role != "assistant" {
		t.Fatalf("final message %+v", got.last.Messages[0])
	}
}

func TestStream_AssemblesToolCallsSplitAcrossChunksInOpenAIShape(t *testing.T) {
	cl := newClient(t, sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"rome\"}"}}]}}]}`,
		`[DONE]`))
	ch, _ := cl.QueryStream(context.Background(), nil, nil)
	final := collect(t, ch).last.Messages[0]
	wire, _ := json.Marshal(final)
	want := `"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"rome\"}"}}]`
	if !strings.Contains(string(wire), want) {
		t.Fatalf("got %s", wire)
	}
}

func TestStream_ErrorPayloadEndsTheStreamWithThatError(t *testing.T) {
	cl := newClient(t, sse(
		`{"choices":[{"delta":{"content":"pa"}}]}`,
		`{"error":{"message":"provider overloaded"}}`))
	ch, _ := cl.QueryStream(context.Background(), nil, nil)
	got := collect(t, ch)
	if got.content != "pa" || got.last.Err == nil || got.last.Err.Error() != "provider overloaded" {
		t.Fatalf("%+v", got)
	}
}

func TestStream_MalformedChunkIsAnError(t *testing.T) {
	cl := newClient(t, sse(`{not json`))
	ch, _ := cl.QueryStream(context.Background(), nil, nil)
	if collect(t, ch).last.Err == nil {
		t.Fatal("expected an error")
	}
}

func TestStream_OutlivesTheRequestTimeout(t *testing.T) {
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 4 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	ch, _ := cl.QueryStream(context.Background(), nil, nil)
	if got := collect(t, ch); got.content != "xxxx" || !got.last.Done {
		t.Fatalf("stream cut short: %+v", got)
	}
}

func TestStream_CancelClosesTheChannel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := cl.QueryStream(ctx, nil, nil)
	<-ch
	cancel()
	done := make(chan struct{})
	go func() { collect(t, ch); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestRequest_SendsTemplateKwargsOnlyWhenThinkingIsConfigured(t *testing.T) {
	var bodies []string
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"T\",\"summary\":\"S\"}"}}]}`)
	}))
	cl.Rename(context.Background(), nil)
	on := true
	cl.Cfg.Llm.Thinking = &on
	cl.Rename(context.Background(), nil)
	if strings.Contains(bodies[0], "chat_template_kwargs") {
		t.Fatal("kwargs sent without Thinking configured")
	}
	if !strings.Contains(bodies[1], `"chat_template_kwargs":{"enable_thinking":false}`) {
		t.Fatalf("rename must force thinking off: %s", bodies[1])
	}
}

func TestRequest_ErrorStatusCarriesTheResponseBody(t *testing.T) {
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"Unrecognized request argument"}}`)
	}))
	_, err := cl.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unrecognized request argument") {
		t.Fatal(err)
	}
}

func TestParseRename(t *testing.T) {
	for _, bad := range []string{"no json", "} {", `{"title":""}`, "{bad}"} {
		if _, err := parseRename(bad); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
	meta, err := parseRename("```json\n{\"title\":\"A\",\"summary\":\"B\"}\n```")
	if err != nil || meta.Title != "A" || meta.Summary != "B" {
		t.Fatal(meta, err)
	}
}

func TestSummarize_FoldsThePreviousSummaryWithTheTitleModel(t *testing.T) {
	var req ChatRequest
	cl := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":" new summary "}}]}`)
	}))
	cl.Cfg.Llm.Model, cl.Cfg.Llm.TitleModel = "big", "cheap"
	got, err := cl.Summarize(context.Background(), "old summary", []model.Message{{Role: "user", Content: "hi"}})
	if err != nil || got != "new summary" {
		t.Fatal(got, err)
	}
	prompt := req.Messages[1].Content
	if req.Model != "cheap" || !strings.Contains(prompt, "Summary so far:\nold summary") || !strings.Contains(prompt, "user: hi") {
		t.Fatalf("%s: %q", req.Model, prompt)
	}
}

func embedServer(t *testing.T, cfg config.EmbeddingConfig, respond func(inputs []string) EmbedResponse) (*Embedder, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req EmbedRequest
		json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req.Input...)
		json.NewEncoder(w).Encode(respond(req.Input))
	}))
	t.Cleanup(srv.Close)
	cfg.Url, cfg.Timeout = srv.URL, 1000
	return NewEmbedder(cfg), &seen
}

func TestEmbedder_OrdersVectorsByIndexAndAppliesPrefixes(t *testing.T) {
	e, seen := embedServer(t, config.EmbeddingConfig{QueryPrefix: "Q: ", DocPrefix: "D: "}, func(in []string) EmbedResponse {
		var resp EmbedResponse
		for i := len(in) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, Embedding{Index: i, Embedding: []float32{float32(i)}})
		}
		return resp
	})
	vecs, err := e.EmbedDocuments(context.Background(), []string{"a", "b"})
	if err != nil || vecs[0][0] != 0 || vecs[1][0] != 1 {
		t.Fatal(vecs, err)
	}
	e.EmbedQuery(context.Background(), "q")
	if strings.Join(*seen, ",") != "D: a,D: b,Q: q" {
		t.Fatal(*seen)
	}
}

func TestEmbedder_MissingVectorIsAnError(t *testing.T) {
	e, _ := embedServer(t, config.EmbeddingConfig{}, func(in []string) EmbedResponse {
		return EmbedResponse{Data: []Embedding{{Index: 0, Embedding: []float32{1}}}}
	})
	if _, err := e.EmbedBatch(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected an error")
	}
}
