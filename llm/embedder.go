package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	config "git.estatecloud.org/radumaco/souvenir/config"
)

const EMBEDDINGS_API = "/v1/embeddings"

type Embedder struct {
	Cfg    config.EmbeddingConfig
	logger slog.Logger
	id     string
	client *http.Client
}

type EmbedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format"`
}

type EmbedResponse struct {
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Object string      `json:"object"`
	Usage  Usage       `json:"usage"`
}

type Embedding struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
	Object    string    `json:"object"`
}

func NewEmbedder(cfg config.EmbeddingConfig) *Embedder {
	return &Embedder{
		Cfg:    cfg,
		logger: *slog.Default().With("Component", "Embedder"),
		client: &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Millisecond},
	}
}

func (e Embedder) Dim() int {
	return e.Cfg.Dim
}

func (e Embedder) ID() string {
	if e.id == "" {
		e.id = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			default:
				return '_'
			}
		}, e.Cfg.Model)
	}
	return e.id
}

func (e Embedder) EmbedBatch(text []string) ([][]float32, error) {
	requestBody := EmbedRequest{
		Model:          e.Cfg.Model,
		Input:          text,
		EncodingFormat: "float",
	}
	serialized, err := json.Marshal(requestBody)
	if err != nil {
		e.logger.Error("There was an error marshelling the request body",
			"request", requestBody,
			"error", err)
		return [][]float32{}, err
	}
	body := bytes.NewBuffer(serialized)
	req, err := http.NewRequest("POST", e.Cfg.Url+EMBEDDINGS_API, body)
	if err != nil {
		e.logger.Error("There was an error creating the request", "error", err.Error())
		return [][]float32{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+e.Cfg.Key)
	}
	e.logger.Debug("Executing embedding call", "inputs", len(text))
	resp, err := e.client.Do(req)
	if err != nil {
		e.logger.Error("There was an error during http call", "error", err.Error())
		return [][]float32{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.logger.Error("There was an error reading the response body", "error", err.Error())
		return [][]float32{}, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		e.logger.Error("Call returned non 200 status",
			"statusCode", resp.StatusCode,
			"status", resp.Status)
		return [][]float32{},
			errors.New("Call returned invalid status " +
				resp.Status)
	}

	var response EmbedResponse
	if err := json.Unmarshal(data, &response); err != nil {
		e.logger.Error("There was an error parsing the response", "error", err.Error())
		return [][]float32{}, err
	}

	if len(response.Data) == 0 {
		e.logger.Error("The call returned empty data")
		return [][]float32{}, errors.New("The call returned empty data")
	}

	result := make([][]float32, len(text))
	for _, embed := range response.Data {
		if embed.Index < 0 || embed.Index >= len(result) {
			return nil, fmt.Errorf("Embedding index out of range. Index:%d Length:%d",
				embed.Index, len(result))
		}
		result[embed.Index] = embed.Embedding
	}
	for i, vec := range result {
		if vec == nil {
			return nil, fmt.Errorf("Embedding missing for input %d of %d", i, len(result))
		}
	}

	return result, nil
}
