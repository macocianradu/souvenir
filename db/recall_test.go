package db_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// fakeEmbedder answers with the vector given for each text.
func fakeEmbedder(t *testing.T, vectors map[string][]float32, cfg config.EmbeddingConfig) *llm.Embedder {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.EmbedRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := llm.EmbedResponse{}
		for i, text := range req.Input {
			vec, ok := vectors[text]
			if !ok {
				t.Errorf("no vector for %q", text)
				vec = []float32{1, 1, 1, 1}
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

	// Eleven memories about the user's job sit close to one another and at
	// much the same distance from any message, as long memories do; one about
	// umbrellas points elsewhere.
	vectors := map[string][]float32{
		"Owns a yellow umbrella":            {0, 0, 0, 1},
		"Does it look like rain":            {0.3, 0, 0, 1},
		"Who wrote the first Naruto volume": {1, 0, 0, 0.3},
	}
	var job []string
	for i := range 11 {
		text := fmt.Sprintf("Salary detail number %d", i)
		job = append(job, text)
		vectors[text] = []float32{1, 0.05 * float32(i%3), 0.05 * float32(i%2), 0}
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
