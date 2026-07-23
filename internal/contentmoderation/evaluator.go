package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ActionAllow        = "allow"
	ActionKeywordBlock = "keyword_block"
	ActionAPIBlock     = "api_block"
	ActionAPIError     = "api_error"
)

type Decision struct {
	WouldBlock      bool               `json:"would_block"`
	Action          string             `json:"action"`
	MatchedKeyword  string             `json:"matched_keyword,omitempty"`
	HighestCategory string             `json:"highest_category,omitempty"`
	HighestScore    float64            `json:"highest_score,omitempty"`
	CategoryScores  map[string]float64 `json:"category_scores"`
	Thresholds      map[string]float64 `json:"thresholds"`
	LatencyMS       int64              `json:"latency_ms"`
	ModerationError string             `json:"moderation_error,omitempty"`
}

type Evaluator struct {
	httpClient *http.Client
}

func NewEvaluator(client *http.Client) *Evaluator {
	if client == nil {
		client = http.DefaultClient
	}
	return &Evaluator{httpClient: client}
}

func (e *Evaluator) Evaluate(ctx context.Context, profile Profile, input string) Decision {
	decision := Decision{Action: ActionAllow, CategoryScores: map[string]float64{}, Thresholds: mergeThresholds(profile.Thresholds)}
	input = strings.TrimSpace(input)
	if profile.Mode != ModePreBlock || input == "" {
		return decision
	}
	if profile.KeywordMode != KeywordModeAPIOnly {
		if keyword, hit := matchKeyword(input, profile.BlockedKeywords); hit {
			decision.WouldBlock = true
			decision.Action = ActionKeywordBlock
			decision.MatchedKeyword = keyword
			return decision
		}
	}
	if profile.KeywordMode == KeywordModeKeywordOnly {
		return decision
	}
	start := time.Now()
	scores, err := e.callModeration(ctx, profile, input)
	decision.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		decision.Action = ActionAPIError
		decision.ModerationError = err.Error()
		return decision
	}
	decision.CategoryScores = scores
	for category, score := range scores {
		if decision.HighestCategory == "" || score > decision.HighestScore {
			decision.HighestCategory = category
			decision.HighestScore = score
		}
		if threshold, ok := decision.Thresholds[category]; ok && score >= threshold {
			decision.WouldBlock = true
		}
	}
	if decision.WouldBlock {
		decision.Action = ActionAPIBlock
	}
	return decision
}

func matchKeyword(input string, keywords []string) (string, bool) {
	input = strings.ToLower(input)
	for _, keyword := range keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword != "" && strings.Contains(input, strings.ToLower(keyword)) {
			return keyword, true
		}
	}
	return "", false
}

func (e *Evaluator) callModeration(ctx context.Context, profile Profile, input string) (map[string]float64, error) {
	endpoint, err := url.JoinPath(strings.TrimRight(profile.BaseURL, "/"), "v1/moderations")
	if err != nil {
		return nil, fmt.Errorf("invalid moderation base URL: %w", err)
	}
	payload, err := json.Marshal(struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{Model: profile.Model, Input: input})
	if err != nil {
		return nil, fmt.Errorf("encode moderation request: %w", err)
	}
	timeout := time.Duration(profile.TimeoutMS) * time.Millisecond
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create moderation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+profile.APIKeySecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moderation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("moderation API returned status %d", resp.StatusCode)
	}
	var result struct {
		Results []struct {
			CategoryScores map[string]float64 `json:"category_scores"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode moderation response: %w", err)
	}
	if len(result.Results) == 0 || result.Results[0].CategoryScores == nil {
		return nil, fmt.Errorf("moderation response missing category scores")
	}
	return result.Results[0].CategoryScores, nil
}
