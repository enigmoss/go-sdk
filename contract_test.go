package enigmoss

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

type signatureVectors struct {
	Request struct {
		Timestamp string            `json:"timestamp"`
		Project   string            `json:"project"`
		Secrets   map[string]string `json:"secrets"`
		Vectors   []struct {
			Name      string `json:"name"`
			Method    string `json:"method"`
			Path      string `json:"path"`
			Body      string `json:"body"`
			BodyBytes int    `json:"body_bytes"`
			KeyType   string `json:"key_type"`
			Signature string `json:"signature"`
		} `json:"vectors"`
	} `json:"request"`
	Webhook struct {
		Vectors []struct {
			Name      string `json:"name"`
			Timestamp string `json:"timestamp"`
			Project   string `json:"project"`
			Secret    string `json:"secret"`
			Body      string `json:"body"`
			Signature string `json:"signature"`
		} `json:"vectors"`
	} `json:"webhook"`
}

func TestRequestSignatureVectors(t *testing.T) {
	var v signatureVectors
	readJSON(t, "testdata/contract/signature-vectors.json", &v)
	if len(v.Request.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, vec := range v.Request.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			if len(vec.Body) != vec.BodyBytes {
				t.Fatalf("body is %d bytes, the vector says %d", len(vec.Body), vec.BodyBytes)
			}
			got := signRequest(v.Request.Secrets[vec.KeyType], v.Request.Timestamp, v.Request.Project, vec.Method, vec.Path, []byte(vec.Body))
			if got != vec.Signature {
				t.Errorf("signature %s, want %s", got, vec.Signature)
			}
		})
	}
}

func TestWebhookSignatureVectors(t *testing.T) {
	var v signatureVectors
	readJSON(t, "testdata/contract/signature-vectors.json", &v)
	if len(v.Webhook.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, vec := range v.Webhook.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			if got := signWebhook(vec.Secret, vec.Timestamp, vec.Project, []byte(vec.Body)); got != vec.Signature {
				t.Errorf("signature %s, want %s", got, vec.Signature)
			}
			ts, _ := strconv.ParseInt(vec.Timestamp, 10, 64)
			h := http.Header{}
			h.Set("X-Project-Uuid", vec.Project)
			h.Set("X-Timestamp", vec.Timestamp)
			h.Set("X-Signature", strings.ToUpper(vec.Signature))
			if err := verifyWebhook([]byte(vec.Body), h, vec.Project, []string{"old-key", vec.Secret}, time.Unix(ts+299, 0)); err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}
}

func TestIdempotencyKeyVectors(t *testing.T) {
	var v struct {
		Vectors []struct {
			Name    string `json:"name"`
			Path    string `json:"path"`
			OrderID string `json:"order_id"`
			Key     string `json:"key"`
		} `json:"vectors"`
	}
	readJSON(t, "testdata/contract/idempotency-key-vectors.json", &v)
	var withQuery bool
	for _, vec := range v.Vectors {
		withQuery = withQuery || strings.Contains(vec.Path, "?")
		t.Run(vec.Name, func(t *testing.T) {
			if got := derivedIdempotencyKey(vec.Path, vec.OrderID); got != vec.Key {
				t.Errorf("key %s, want %s", got, vec.Key)
			}
			if len(vec.Key) != 64 {
				t.Errorf("key is %d characters", len(vec.Key))
			}
		})
	}
	if len(v.Vectors) == 0 || !withQuery {
		t.Error("the vectors must include a path with a query string")
	}
}

func TestNoDependencies(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 0 && (f[0] == "require" || f[0] == "toolchain") {
			t.Errorf("go.mod must stay without dependencies and toolchain: %q", line)
		}
	}
	if _, err := os.Stat("go.sum"); err == nil {
		t.Error("go.sum must not exist")
	}
}
