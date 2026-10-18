package memory

import (
	"context"
	"math"
	"sync"
)

// decoys read like saved memories about someone else's life. A message lands
// at much the same distance from them as from any memory it has nothing to do
// with, so they show how close an unrelated memory comes to that message, for
// a user with few memories as well as many. They span unrelated topics, so
// one sharing a message's topic barely moves their spread.
var decoys = []string{
	"The user keeps three chickens in their back garden and sells the eggs to neighbours.",
	"The user is learning to play the cello and practises for half an hour most evenings.",
	"The user is allergic to penicillin and carries a card saying so.",
	"The user supports a second-division football club and holds a season ticket.",
	"The user's sister lives in Lisbon and works as a dentist.",
	"The user prefers aisle seats on flights longer than two hours.",
	"The user is renovating the attic of their house into a home office, planned to finish next spring.",
	"The user runs a small online shop selling hand-dyed wool.",
	"The user does not drink coffee after noon because it keeps them awake.",
	"The user has a grey cat called Pixel who is fourteen years old.",
	"The user's bank charges a monthly fee they want to get rid of by switching accounts.",
	"The user went hiking in the Dolomites last summer and wants to return with friends.",
	"The user writes software in Rust at work and prefers tabs over spaces.",
	"The user volunteers at a food bank every second Saturday.",
	"The user's landlord raises the rent each January, by the yearly index.",
	"The user is vegetarian but eats fish once or twice a month.",
	"The user is saving for a sailing course and has put aside about half the cost.",
	"The user's daughter starts secondary school next September and needs a new bicycle.",
	"The user collects vinyl records of 1970s jazz.",
	"The user had knee surgery two years ago and avoids running on hard surfaces.",
	"The user's phone contract ends in March and they are comparing offers.",
	"The user speaks Dutch at home, English at work and is learning Japanese.",
	"The user grows tomatoes and basil on a south-facing balcony.",
	"The user takes the night train when visiting their parents in Vienna.",
	"The user is training for a half marathon in the spring, running four times a week.",
	"The user dislikes horror films but enjoys long historical dramas.",
	"The user's car insurance renews in June and went up by a fifth last year.",
	"The user plays chess online in the evenings, mostly ten-minute games.",
	"The user is planning a small wedding for about forty guests next autumn.",
	"The user keeps a paper diary and buys the same notebook brand every year.",
	"The user would like to move closer to the sea once their children have left home.",
	"The user bakes sourdough bread every weekend and keeps the starter in the fridge.",
}

// decoyVectors embeds the decoys once; a failure is retried on the next call.
type decoyVectors struct {
	mu      sync.Mutex
	vectors [][]float32
}

func (s *Store) decoyVectors(ctx context.Context) ([][]float32, error) {
	s.decoys.mu.Lock()
	defer s.decoys.mu.Unlock()
	if s.decoys.vectors == nil {
		vecs, err := s.embedder.EmbedDocuments(ctx, decoys)
		if err != nil {
			return nil, err
		}
		s.decoys.vectors = vecs
	}
	return s.decoys.vectors, nil
}

// spread is the mean and standard deviation of the cosine distances from
// query to vectors.
func spread(query []float32, vectors [][]float32) (mean, std float64) {
	distances := make([]float64, len(vectors))
	for i, v := range vectors {
		distances[i] = cosineDistance(query, v)
		mean += distances[i]
	}
	mean /= float64(len(distances))
	for _, d := range distances {
		std += (d - mean) * (d - mean)
	}
	return mean, math.Sqrt(std / float64(len(distances)))
}

func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/math.Sqrt(na*nb)
}
