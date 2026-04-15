package mobilebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type WorkerHeartbeatPublisher struct {
	client        *http.Client
	server        *WorkerControlServer
	heartbeatURL  string
	token         string
	workerID      string
	hostname      string
	advertiseAddr string
	interval      time.Duration
}

func NewWorkerHeartbeatPublisher(server *WorkerControlServer, heartbeatURL, token, workerID, hostname, advertiseAddr string, interval time.Duration) *WorkerHeartbeatPublisher {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &WorkerHeartbeatPublisher{
		client:        &http.Client{Timeout: 10 * time.Second},
		server:        server,
		heartbeatURL:  heartbeatURL,
		token:         token,
		workerID:      workerID,
		hostname:      hostname,
		advertiseAddr: advertiseAddr,
		interval:      interval,
	}
}

func (p *WorkerHeartbeatPublisher) Run(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Send(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.Send(ctx); err != nil {
				return err
			}
		}
	}
}

func (p *WorkerHeartbeatPublisher) Send(ctx context.Context) error {
	if p == nil || p.server == nil {
		return fmt.Errorf("mobilebridge: heartbeat publisher requires a worker control server")
	}
	if p.heartbeatURL == "" {
		return fmt.Errorf("mobilebridge: heartbeat url is required")
	}
	if p.workerID == "" {
		return fmt.Errorf("mobilebridge: worker id is required")
	}
	payload := p.server.Snapshot(ctx, p.workerID, p.hostname, p.advertiseAddr)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.heartbeatURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mobilebridge: heartbeat failed: status %d", resp.StatusCode)
	}
	return nil
}
