package db_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db"
	"git.estatecloud.org/radumaco/souvenir/db/dbtest"
	"git.estatecloud.org/radumaco/souvenir/db/memory"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/llm"
)

func fakeEmbedder(t *testing.T, vectors map[string][]float32, cfg config.EmbeddingConfig) *llm.Embedder {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.EmbedRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := llm.EmbedResponse{}
		for i, text := range req.Input {
			vec, ok := vectors[text]
			if !ok {
				vec = generic(text)
			}
			resp.Data = append(resp.Data, llm.Embedding{Embedding: vec, Index: i})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	cfg.Url, cfg.Model, cfg.Dim, cfg.BatchSize, cfg.Timeout = srv.URL, "fake", 4, 64, 5000
	return llm.NewEmbedder(cfg)
}

func TestRecall_OnlyMemoriesThatStandOut(t *testing.T) {
	pool := dbtest.Pool(t, "recall")
	ctx := context.Background()
	if err := db.EnsureVectorExtension(ctx, pool, "recall"); err != nil {
		t.Skip(err)
	}
	owner, err := users.NewStore(pool).Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx = users.WithUser(ctx, owner)

	vectors := map[string][]float32{
		"Owns a yellow umbrella":            {0, 0, 0, 1},
		"Does it look like rain":            {0.3, 0, 0, 1},
		"Who wrote the first Naruto volume": {1, 0, 0, 0.3},
	}
	var job []string
	for i := range 5 {
		job = append(job, fmt.Sprintf("Salary detail number %d", i))
	}
	embedder := fakeEmbedder(t, vectors, config.EmbeddingConfig{RecallDeviations: 3})
	store, err := memory.NewStore(ctx, pool, embedder)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range append(job, "Owns a yellow umbrella") {
		if _, _, err := store.Save(ctx, text, ""); err != nil {
			t.Fatal(err)
		}
	}

	found, err := store.Recall(ctx, "Who wrote the first Naruto volume", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("an unrelated message recalled %d memories, first %q", len(found), found[0].Content)
	}

	found, err = store.Recall(ctx, "Does it look like rain", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Content != "Owns a yellow umbrella" {
		t.Errorf("recalled %v, want only the umbrella", found)
	}

	// Searching on request is not held to standing out.
	if found, _ := store.Search(ctx, "Who wrote the first Naruto volume", 5); len(found) != 5 {
		t.Errorf("search found %d, want 5", len(found))
	}
}

// generic is a vector near (1, 0, 0, 0), nudged by the text's hash.
func generic(text string) []float32 {
	h := fnv.New32a()
	h.Write([]byte(text))
	sum := h.Sum32()
	return []float32{1, float32(sum%100) / 1000, float32(sum/100%100) / 1000, 0}
}
