package procfs

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

const dockerSocket = "/var/run/docker.sock"

const dockerRequestTimeout = time.Second

func (r *Reader) dockerImageName(id string) (string, bool) {
	socket := r.DockerSocket
	if socket == "" {
		socket = dockerSocket
	}
	client := &http.Client{
		Timeout: dockerRequestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+id+"/json", nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Config.Image == "" {
		return "", false
	}
	return body.Config.Image, true
}
