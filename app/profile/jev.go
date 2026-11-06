package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"
)

// jev defaults. the model is pinned: an alias like jev-latest moves under calibrated thresholds
const (
	DefaultJevAPIBase = "https://api.typesafe.ai/v1/systemone"
	DefaultJevModel   = "jev-1.14.0"
	DefaultJevTimeout = 3 * time.Second

	maxJevResponse    = 1 << 20
	maxErrorBodyRunes = 300 // an error body of jev goes to logs and the decision reason
)

// JevConfig is the connection to the jev decision API
type JevConfig struct {
	Token   string // bearer credential
	APIBase string // empty falls back to the default endpoint
	Model   string // pinned version, never an alias
	Timeout time.Duration
}

func (c *JevConfig) validate() error {
	if c.Token == "" {
		return errors.New("jev token must be set")
	}
	if c.Model == "" || c.Model == "jev-latest" || c.Model == "jev-preview" {
		return fmt.Errorf("jev model %q must be a pinned version, not an alias", c.Model)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("jev timeout (%v) must be positive", c.Timeout)
	}
	if c.APIBase == "" {
		c.APIBase = DefaultJevAPIBase
	}
	return nil
}

// HTTPDoer is the subset of http.Client the jev client needs
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Question is a jev question. Type is noul (yes/no), choice or score.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Image is a picture referenced from the state by Ref
type Image struct {
	Ref  string
	Data []byte // jpeg or png
}

// Request is one jev call: a state, named questions and the images the state refers to
type Request struct {
	State     any
	Questions map[string]Question
	Images    []Image
}

// Answer is one answer of jev. Pointers keep a missing value apart from an explicit zero.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Response is the parsed jev reply
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// HTTPError is a non-2xx reply of the API
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("jev status %d: %s", e.Status, e.Body) }

type jevAsset struct {
	Type string `json:"type"`
	Part string `json:"part"`
}

type jevBody struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	Assets    map[string]jevAsset `json:"assets,omitempty"`
}

// jevClient talks to the typesafe.ai decision API
type jevClient struct {
	http HTTPDoer
	cfg  JevConfig
}

func newJevClient(client HTTPDoer, cfg JevConfig) (*jevClient, error) {
	if client == nil {
		return nil, errors.New("jev http client must not be nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &jevClient{http: client, cfg: cfg}, nil
}

// ask sends the request and returns the answers. The configured timeout covers the whole call.
func (c *jevClient) ask(ctx context.Context, r Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	body := jevBody{Model: c.cfg.Model, State: r.State, Questions: r.Questions}
	if len(r.Images) > 0 {
		body.Assets = make(map[string]jevAsset, len(r.Images))
		for i, img := range r.Images {
			body.Assets[img.Ref] = jevAsset{Type: "image", Part: fmt.Sprintf("photo%d", i)}
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("can't marshal jev request: %w", err)
	}

	reader, contentType := io.Reader(bytes.NewReader(payload)), "application/json"
	if len(r.Images) > 0 {
		if reader, contentType, err = multipartBody(payload, r.Images); err != nil {
			return Response{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase, reader)
	if err != nil {
		return Response{}, fmt.Errorf("can't make jev request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", contentType)
	if len(r.Images) > 0 {
		req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("jev request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing useful to do with a close error on a read body
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJevResponse))
	if err != nil {
		return Response{}, fmt.Errorf("can't read jev response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, &HTTPError{Status: resp.StatusCode, Body: trunc(string(data), maxErrorBodyRunes)}
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, fmt.Errorf("can't parse jev response: %w", err)
	}
	return out, nil
}

// multipartBody puts the json request in the part "request" and every image in its own file part
func multipartBody(payload []byte, images []Image) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="request"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", fmt.Errorf("can't make request part: %w", err)
	}
	if _, err = part.Write(payload); err != nil {
		return nil, "", fmt.Errorf("can't write request part: %w", err)
	}
	for i, img := range images {
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo%d"; filename="photo%d.jpg"`, i, i))
		fh.Set("Content-Type", "image/jpeg")
		if part, err = mw.CreatePart(fh); err != nil {
			return nil, "", fmt.Errorf("can't make image part: %w", err)
		}
		if _, err = part.Write(img.Data); err != nil {
			return nil, "", fmt.Errorf("can't write image part: %w", err)
		}
	}
	if err = mw.Close(); err != nil {
		return nil, "", fmt.Errorf("can't close multipart body: %w", err)
	}
	return &buf, mw.FormDataContentType(), nil
}
