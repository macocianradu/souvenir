package db_test

import (
	"context"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/dbtest"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/db/memory"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	return dbtest.Pool(t, "scoping")
}

func TestScoping_UsersSeeOnlyTheirOwnRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	accounts := users.NewStore(pool)
	if _, err := accounts.Owner(ctx); err != nil {
		t.Fatal("the migration did not create the owner:", err)
	}
	alice, err := accounts.Add(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := accounts.Add(ctx, "bob", false)
	if err != nil {
		t.Fatal(err)
	}
	asAlice, asBob := users.WithUser(ctx, alice), users.WithUser(ctx, bob)

	hist := history.New(pool, config.DbConfig{ChunkSize: 50, ChunkOverlap: 10})
	conv, err := hist.SaveConversation(asAlice, model.Conversation{Title: "Aubergines", Messages: []model.Message{
		{Role: "user", Content: "how do I grow aubergines on a balcony", Seq: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	memories, _ := memory.NewStore(ctx, pool, nil)
	mem, _, err := memories.Save(asAlice, "Alice grows aubergines", conv.Id)
	if err != nil {
		t.Fatal(err)
	}
	searcher := search.New(pool, nil)

	if _, err := hist.GetConversation(asBob, conv.Id); err == nil {
		t.Error("bob opened alice's conversation")
	}
	if list, _ := hist.GetConversations(asBob); len(list) != 0 {
		t.Error("bob listed alice's conversation")
	}
	if hits, _ := searcher.Search(asBob, "aubergines", search.Options{}); len(hits) != 0 {
		t.Error("bob found alice's messages")
	}
	if err := hist.SaveContextSummary(asBob, conv.Id, model.ContextSummary{ThroughSeq: 1, Content: "x"}); err == nil {
		t.Error("bob summarized alice's conversation")
	}
	if _, err := hist.SaveConversation(asBob, model.Conversation{Id: conv.Id, Title: "Mine"}); err == nil {
		t.Error("bob wrote to alice's conversation")
	}
	if err := hist.DeleteConversation(asBob, conv.Id); err == nil {
		t.Error("bob deleted alice's conversation")
	}
	if list, _ := memories.List(asBob); len(list) != 0 {
		t.Error("bob listed alice's memories")
	}
	if found, _ := memories.Search(asBob, "aubergines", 5); len(found) != 0 {
		t.Error("bob recalled alice's memory")
	}
	if _, err := memories.Update(asBob, mem.Id, "changed"); err == nil {
		t.Error("bob changed alice's memory")
	}
	if err := memories.Forget(asBob, mem.Id); err == nil {
		t.Error("bob forgot alice's memory")
	}
	if _, created, err := memories.Save(asBob, "Alice grows aubergines", conv.Id); err != nil || !created {
		t.Error("the same memory text should be saved separately for bob:", err)
	}

	got, err := hist.GetConversation(asAlice, conv.Id)
	if err != nil || got.Title != "Aubergines" || len(got.Messages) != 1 {
		t.Fatalf("alice lost her conversation: %+v %v", got, err)
	}
	if hits, _ := searcher.Search(asAlice, "aubergines", search.Options{}); len(hits) != 1 {
		t.Error("alice cannot find her own message")
	}
	if found, _ := memories.Search(asAlice, "aubergines", 5); len(found) != 1 {
		t.Error("alice cannot recall her own memory")
	}

	if _, err := hist.GetConversations(ctx); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Error("a query without a user was not refused")
	}
}

func TestUsers_KeysAndTheLastAdmin(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	accounts := users.NewStore(pool)
	if _, err := accounts.Add(ctx, "carol", false); err != nil {
		t.Fatal(err)
	}
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl carol@laptop"
	key, err := accounts.AddKey(ctx, "carol", line)
	if err != nil {
		t.Fatal(err)
	}
	if key.Comment != "carol@laptop" {
		t.Errorf("comment %q", key.Comment)
	}
	if _, err := accounts.AddKey(ctx, "carol", line); err == nil {
		t.Error("the same key was registered twice")
	}
	if _, err := accounts.AddKey(ctx, "nobody", line); err == nil {
		t.Error("a key was registered for a missing user")
	}
	if err := accounts.Remove(ctx, "owner"); err == nil {
		t.Error("the last admin was removed")
	}
	if err := accounts.Remove(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := accounts.Keys(ctx); len(keys) != 0 {
		t.Error("removing a user kept their keys")
	}
}

func TestToolRounds_SurviveARoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner, err := users.NewStore(pool).Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx = users.WithUser(ctx, owner)
	hist := history.New(pool, config.DbConfig{ChunkSize: 50, ChunkOverlap: 10})
	call := model.ToolCall{Id: "call_1", Type: "function", Function: model.FunctionCall{Name: "web_search", Arguments: `{"query":"x"}`}}
	conv, err := hist.SaveConversation(ctx, model.Conversation{Messages: []model.Message{
		{Role: "user", Content: "look it up", Seq: 1},
		{Role: "assistant", ToolCalls: []model.ToolCall{call}, Seq: 2},
		{Role: "tool", ToolCallId: "call_1", Content: "found", Seq: 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := hist.GetConversation(ctx, conv.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || len(got.Messages[1].ToolCalls) != 1 || got.Messages[1].ToolCalls[0] != call ||
		got.Messages[2].ToolCallId != "call_1" || got.Messages[0].ToolCalls != nil || got.Messages[0].ToolCallId != "" {
		t.Fatalf("%+v", got.Messages)
	}
}

func TestReplaceToolResult_OnlyOnceAndOnlyForTheOwner(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	accounts := users.NewStore(pool)
	alice, err := accounts.Add(ctx, "carol", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := accounts.Add(ctx, "dave", false)
	if err != nil {
		t.Fatal(err)
	}
	asAlice, asBob := users.WithUser(ctx, alice), users.WithUser(ctx, bob)
	hist := history.New(pool, config.DbConfig{ChunkSize: 50, ChunkOverlap: 10})
	call := model.ToolCall{Id: "call_1", Type: "function", Function: model.FunctionCall{Name: "calendar_create", Arguments: `{}`}}
	conv, err := hist.SaveConversation(asAlice, model.Conversation{Messages: []model.Message{
		{Role: "user", Content: "add it", Seq: 1},
		{Role: "assistant", ToolCalls: []model.ToolCall{call}, Seq: 2},
		{Role: "tool", ToolCallId: "call_1", Content: model.Held + ", because…", Seq: 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := hist.GetConversation(asAlice, conv.Id)
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Messages[2].Id
	if ok, _ := hist.ReplaceToolResult(asBob, conv.Id, id, model.Held, model.Running); ok {
		t.Fatal("dave replaced carol's tool result")
	}
	if ok, err := hist.ReplaceToolResult(asAlice, conv.Id, id, model.Held, model.Running); !ok || err != nil {
		t.Fatal("the owner could not claim the held call", err)
	}
	if ok, _ := hist.ReplaceToolResult(asAlice, conv.Id, id, model.Held, model.Running); ok {
		t.Fatal("a held call was claimed twice")
	}
	if ok, _ := hist.ReplaceToolResult(asAlice, conv.Id, saved.Messages[0].Id, "", "x"); ok {
		t.Fatal("a user message was rewritten")
	}
}
