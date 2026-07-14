package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const usageBufferTokens = 2000

type upstreamChatResponse struct {
	Response *http.Response
	URL      string
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

var chatTransportCache sync.Map // timeout duration → *http.Client

func chatHTTPClient(timeout time.Duration) *http.Client {
	if cached, ok := chatTransportCache.Load(timeout); ok {
		return cached.(*http.Client)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = timeout
	client := &http.Client{Transport: base}
	actual, _ := chatTransportCache.LoadOrStore(timeout, client)
	return actual.(*http.Client)
}

// executeNativeUpstream mirrors BaseExecutor's generic HTTP path for the strict
// native transport subset: POST JSON, static headers + Bearer auth, client
// cancellation, response-header timeout, and default 502/503/504 retries.
func executeNativeUpstream(ctx context.Context, resolution resolvedNativeChat, body map[string]any, doer httpDoer) (upstreamChatResponse, error) {
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["model"] = resolution.UpstreamModel
	payload, err := json.Marshal(out)
	if err != nil {
		return upstreamChatResponse{}, fmt.Errorf("encode upstream request: %w", err)
	}

	timeout := time.Duration(resolution.Transport.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if doer == nil {
		doer = chatHTTPClient(timeout)
	}
	attempts := map[int]int{http.StatusBadGateway: 3, http.StatusServiceUnavailable: 3, http.StatusGatewayTimeout: 2}
	delays := map[int]time.Duration{http.StatusBadGateway: 3 * time.Second, http.StatusServiceUnavailable: 2 * time.Second, http.StatusGatewayTimeout: 3 * time.Second}
	remainingNetworkRetries := 3

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolution.Transport.BaseURL, bytes.NewReader(payload))
		if err != nil {
			return upstreamChatResponse{}, fmt.Errorf("build upstream request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range resolution.Transport.Headers {
			req.Header.Set(k, v)
		}
		token := resolution.Credential
		if resolution.Transport.AuthScheme == "bearer" {
			token = "Bearer " + token
		}
		header := resolution.Transport.AuthHeader
		if header == "" {
			header = "Authorization"
		}
		req.Header.Set(header, token)
		if streamRequested(body) {
			req.Header.Set("Accept", "text/event-stream")
		}

		resp, err := doer.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return upstreamChatResponse{}, ctx.Err()
			}
			if remainingNetworkRetries > 0 {
				remainingNetworkRetries--
				if err := sleepContext(ctx, 3*time.Second); err != nil {
					return upstreamChatResponse{}, err
				}
				continue
			}
			return upstreamChatResponse{}, fmt.Errorf("upstream fetch: %w", err)
		}

		if left := attempts[resp.StatusCode]; left > 0 {
			attempts[resp.StatusCode] = left - 1
			_ = resp.Body.Close()
			if err := sleepContext(ctx, delays[resp.StatusCode]); err != nil {
				return upstreamChatResponse{}, err
			}
			continue
		}
		return upstreamChatResponse{Response: resp, URL: resolution.Transport.BaseURL}, nil
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func streamRequested(body map[string]any) bool {
	v, _ := body["stream"].(bool)
	return v
}

func parseUpstreamError(resp *http.Response) (int, string) {
	status := resp.StatusCode
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil {
		if errObj, ok := obj["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				return status, msg
			}
		}
		if msg, ok := obj["message"].(string); ok && msg != "" {
			return status, msg
		}
		if s, ok := obj["error"].(string); ok && s != "" {
			return status, s
		}
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return status, msg
}

// normalizeNonStreaming applies the wire-visible OpenAI same-format fixes from
// nonStreamingHandler.js. It intentionally does not translate formats.
func normalizeNonStreaming(raw []byte) ([]byte, map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, err
	}
	if _, ok := obj["object"]; !ok {
		obj["object"] = "chat.completion"
	}
	if _, ok := obj["created"]; !ok {
		obj["created"] = time.Now().Unix()
	}
	delete(obj, "prompt_filter_results")
	if choices, ok := obj["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			delete(choice, "content_filter_results")
			msg, _ := choice["message"].(map[string]any)
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				choice["finish_reason"] = "tool_calls"
			}
			if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
				if content, _ := msg["content"].(string); content != "" {
					delete(msg, "reasoning_content")
				}
			}
		}
	}
	usage, _ := obj["usage"].(map[string]any)
	if usage != nil {
		addUsageBuffer(usage)
	}
	out, err := json.Marshal(obj)
	return out, usage, err
}

func addUsageBuffer(usage map[string]any) {
	if n, ok := jsonNumber(usage["prompt_tokens"]); ok {
		usage["prompt_tokens"] = n + usageBufferTokens
	}
	if n, ok := jsonNumber(usage["total_tokens"]); ok {
		usage["total_tokens"] = n + usageBufferTokens
	} else if p, okP := jsonNumber(usage["prompt_tokens"]); okP {
		if c, okC := jsonNumber(usage["completion_tokens"]); okC {
			usage["total_tokens"] = p + c
		}
	}
}

func jsonNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

type streamUsage struct {
	Prompt        int64
	Completion    int64
	Cached        int64
	Reasoning     int64
	CacheCreation int64
	Valid         bool
}

// relayOpenAISSE mirrors the same-format passthrough's client-visible fixes
// while streaming one event at a time. It returns accumulated raw usage (before
// the +2000 client buffer) for persistence.
func relayOpenAISSE(ctx context.Context, w http.ResponseWriter, body io.Reader, requestBody map[string]any) (streamUsage, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return streamUsage{}, errors.New("streaming unsupported by response writer")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	usage := streamUsage{}
	contentChars := 0
	doneSeen := false
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return usage, ctx.Err()
		default:
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			doneSeen = true
			if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
				return usage, err
			}
			flusher.Flush()
			continue
		}
		var chunk map[string]any
		if payload == "" || json.Unmarshal([]byte(payload), &chunk) != nil {
			continue // JS passthrough drops non-JSON data lines
		}
		if id, _ := chunk["id"].(string); id == "chat" || id == "completion" || (id != "" && len(id) < 8) {
			fallback := strconv.FormatInt(time.Now().UnixNano(), 36)
			if ext, _ := chunk["extend_fields"].(map[string]any); ext != nil {
				if v, _ := ext["requestId"].(string); v != "" {
					fallback = v
				} else if v, _ := ext["traceId"].(string); v != "" {
					fallback = v
				}
			}
			chunk["id"] = "chatcmpl-" + fallback
		}
		choices, hasChoices := chunk["choices"].([]any)
		if hasChoices {
			if _, ok := chunk["object"]; !ok {
				chunk["object"] = "chat.completion.chunk"
			}
			if _, ok := chunk["created"]; !ok {
				chunk["created"] = time.Now().Unix()
			}
		}
		delete(chunk, "prompt_filter_results")
		finish := false
		valuable := !hasChoices
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			delete(choice, "content_filter_results")
			delta, _ := choice["delta"].(map[string]any)
			if calls, ok := delta["tool_calls"].([]any); ok {
				if len(calls) == 0 {
					delete(delta, "tool_calls")
				} else {
					valuable = true
				}
			}
			if s, _ := delta["content"].(string); s != "" {
				contentChars += len(s)
				valuable = true
			}
			if s, _ := delta["reasoning_content"].(string); s != "" {
				contentChars += len(s)
				valuable = true
			}
			if role, _ := delta["role"].(string); role != "" {
				valuable = true
			}
			if choice["finish_reason"] != nil {
				finish = true
				valuable = true
			}
		}
		if !valuable {
			continue
		}
		if rawUsage, ok := chunk["usage"].(map[string]any); ok {
			parsed := usageFromMap(rawUsage)
			if parsed.Prompt > usage.Prompt {
				usage.Prompt = parsed.Prompt
			}
			if parsed.Completion > usage.Completion {
				usage.Completion = parsed.Completion
			}
			if parsed.Cached > usage.Cached {
				usage.Cached = parsed.Cached
			}
			if parsed.Reasoning > usage.Reasoning {
				usage.Reasoning = parsed.Reasoning
			}
			if parsed.CacheCreation > usage.CacheCreation {
				usage.CacheCreation = parsed.CacheCreation
			}
			usage.Valid = usage.Prompt > 0 || usage.Completion > 0
		}
		if finish {
			clientUsage := map[string]any{}
			if usage.Valid {
				clientUsage["prompt_tokens"] = usage.Prompt
				clientUsage["completion_tokens"] = usage.Completion
				clientUsage["total_tokens"] = usage.Prompt + usage.Completion
				if usage.Cached > 0 {
					clientUsage["cached_tokens"] = usage.Cached
				}
				if usage.Reasoning > 0 {
					clientUsage["reasoning_tokens"] = usage.Reasoning
				}
				if usage.CacheCreation > 0 {
					clientUsage["cache_creation_input_tokens"] = usage.CacheCreation
				}
			} else {
				usage.Prompt = estimateJSONTokens(requestBody)
				usage.Completion = int64(contentChars / 4)
				if contentChars > 0 && usage.Completion == 0 {
					usage.Completion = 1
				}
				usage.Valid = usage.Prompt > 0 || usage.Completion > 0
				clientUsage["prompt_tokens"] = usage.Prompt
				clientUsage["completion_tokens"] = usage.Completion
				clientUsage["total_tokens"] = usage.Prompt + usage.Completion
				clientUsage["estimated"] = true
			}
			addUsageBuffer(clientUsage)
			chunk["usage"] = clientUsage
		}
		encoded, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(encoded)+"\n\n"); err != nil {
			return usage, err
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		return usage, err
	}
	if !usage.Valid && contentChars > 0 {
		usage.Prompt = estimateJSONTokens(requestBody)
		usage.Completion = int64(contentChars / 4)
		if usage.Completion == 0 {
			usage.Completion = 1
		}
		usage.Valid = usage.Prompt > 0 || usage.Completion > 0
	}
	if !doneSeen {
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return usage, err
	}
	return usage, nil
}

func estimateJSONTokens(body map[string]any) int64 {
	raw, err := json.Marshal(body)
	if err != nil || len(raw) == 0 {
		return 0
	}
	return int64((len(raw) + 3) / 4)
}

func retryAfterSeconds(until time.Time) string {
	remaining := time.Until(until)
	seconds := int64((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
