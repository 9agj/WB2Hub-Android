package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Usage is the token accounting the upstream reports for one completion.
type Usage struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credits          float64 `json:"credits"`
}

// StreamResult summarises what a chat call produced.
type StreamResult struct {
	// WroteAnything records whether any byte reached the client. Once a stream
	// has started, an error can no longer be reported as an HTTP status — the
	// status line is already sent — so the caller needs to know which failure
	// mode it is in.
	WroteAnything bool
	StatusCode    int
	Usage         *Usage
	Frames        int
}

// PostJSON issues a JSON POST and returns the status code.
//
// It is the shape the check-in and balance endpoints need: they answer with a
// small envelope whose body the caller usually does not care about, only
// whether it succeeded.
func (c *Client) PostJSON(ctx context.Context, client *http.Client, url string,
	headers http.Header, payload any) (int, error) {

	_, _, status, err := c.DoJSON(ctx, client, http.MethodPost, url, headers, payload)
	return status, err
}

// StreamChat forwards a chat completion to the upstream and relays the response.
//
// When stream is true the upstream answers with server-sent events and each
// frame is written straight through, so the client sees tokens as they arrive
// rather than after the whole completion buffers. When it is false the
// response is a single JSON document and is copied verbatim.
//
// The relay is deliberately byte-oriented: this gateway is a proxy, and
// re-encoding the frame stream would drop whatever fields a newer client
// understands that we do not.
func (c *Client) StreamChat(ctx context.Context, client *http.Client, url string,
	headers http.Header, payload any, w http.ResponseWriter, stream bool) (StreamResult, error) {

	result := StreamResult{}
	if client == nil {
		client = c.HTTP
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return result, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	for k, values := range headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode >= 400 {
		// Read the error body so the caller can surface upstream's own wording;
		// it is far more useful than a generic message.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		text := strings.TrimSpace(string(body))
		if text == "" {
			text = http.StatusText(resp.StatusCode)
		}
		return result, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, truncate(text, 400))
	}

	flusher, _ := w.(http.Flusher)

	if !stream {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return result, err
		}
		result.Usage = extractUsage(body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		n, _ := w.Write(body)
		result.WroteAnything = n > 0
		if flusher != nil {
			flusher.Flush()
		}
		return result, nil
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var usageBuf bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			n, writeErr := w.Write(line)
			if n > 0 {
				result.WroteAnything = true
			}
			// Keep the tail of the stream so usage can be recovered from it
			// without buffering the whole response.
			if usageBuf.Len() < 256*1024 {
				usageBuf.Write(line)
			}
			if flusher != nil {
				flusher.Flush()
			}
			if writeErr != nil {
				// The client hung up. Not an upstream failure; stop quietly.
				return result, nil
			}
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
				result.Frames++
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if result.WroteAnything {
				// Mid-stream breakage cannot be reported as an HTTP error now.
				return result, nil
			}
			return result, err
		}
	}

	result.Usage = extractUsage(usageBuf.Bytes())
	return result, nil
}

// extractUsage pulls the token accounting out of a response body.
//
// The usage block appears as a top-level "usage" object in a non-streamed
// response and inside the final SSE frame of a streamed one, so both are
// scanned. A missing block returns nil rather than a zero Usage, because "no
// accounting reported" and "reported zero tokens" are different facts and the
// quota tracker must not treat the former as the latter.
func extractUsage(body []byte) *Usage {
	if len(body) == 0 {
		return nil
	}

	// Non-streamed: parse the whole document.
	var doc struct {
		Usage *struct {
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			TotalTokens      int64   `json:"total_tokens"`
			Credits          float64 `json:"credits"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Usage != nil {
		return &Usage{
			PromptTokens:     doc.Usage.PromptTokens,
			CompletionTokens: doc.Usage.CompletionTokens,
			TotalTokens:      doc.Usage.TotalTokens,
			Credits:          doc.Usage.Credits,
		}
	}

	// Streamed: scan data: frames from the end, since usage rides the last one.
	lines := bytes.Split(body, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var frame struct {
			Usage *struct {
				PromptTokens     int64   `json:"prompt_tokens"`
				CompletionTokens int64   `json:"completion_tokens"`
				TotalTokens      int64   `json:"total_tokens"`
				Credits          float64 `json:"credits"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &frame) == nil && frame.Usage != nil {
			return &Usage{
				PromptTokens:     frame.Usage.PromptTokens,
				CompletionTokens: frame.Usage.CompletionTokens,
				TotalTokens:      frame.Usage.TotalTokens,
				Credits:          frame.Usage.Credits,
			}
		}
	}
	return nil
}

// ParseRetryAfter reads a Retry-After header, accepting both the delta-seconds
// and the HTTP-date forms the upstream has been observed to send.
func ParseRetryAfter(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return seconds, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delta := int(timeUntil(when).Seconds())
		if delta < 0 {
			delta = 0
		}
		return delta, true
	}
	return 0, false
}

// timeUntil is indirected so tests can pin the clock.
var timeUntil = func(t time.Time) time.Duration { return time.Until(t) }
