// Package schemareg is a minimal Confluent-compatible schema registry client,
// matching the one in ble-tape-gateway.
package schemareg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 10 * time.Second}}
}

// EnsureProtobuf registers schema under subject if absent and returns its id.
// Retries for ~30s: on a fresh cluster the registry often starts after us.
func (c *Client) EnsureProtobuf(subject, schema string) (int32, error) {
	payload, _ := json.Marshal(map[string]string{
		"schemaType": "PROTOBUF",
		"schema":     schema,
	})
	var lastBody string
	for i := 0; i < 30; i++ {
		resp, err := c.http.Post(
			c.baseURL+"/subjects/"+subject+"/versions",
			"application/vnd.schemaregistry.v1+json",
			bytes.NewReader(payload),
		)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var result struct {
			ID      int32  `json:"id"`
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if result.ID > 0 {
			return result.ID, nil
		}
		lastBody = result.Message
		time.Sleep(time.Second)
	}
	if lastBody != "" {
		return 0, fmt.Errorf("schema registry rejected the schema: %s", lastBody)
	}
	return 0, fmt.Errorf("schema registry not reachable after 30s")
}
