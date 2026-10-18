package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

var (
	ErrNotDemoMode     = errors.New("chaos controller is strictly forbidden outside STEADFAST_ENV=demo")
	ErrContainerDenied = errors.New("target container is not allowed for chaos")
)

type Config struct {
	Env           string
	AdminKey      string
	LedgerBaseURL string
	DockerCmd     string
}

type Controller struct {
	cfg        Config
	httpClient *http.Client
}

func NewController(cfg Config) (*Controller, error) {
	if cfg.Env != "demo" {
		return nil, ErrNotDemoMode
	}
	if cfg.DockerCmd == "" {
		cfg.DockerCmd = "docker"
	}
	if cfg.LedgerBaseURL == "" {
		cfg.LedgerBaseURL = "http://127.0.0.1:8082"
	}
	return &Controller{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (c *Controller) VerifyAllowed(ctx context.Context, nameOrID string) error {
	// Explicit allowlist of container names
	allowedNames := map[string]bool{
		"steadfast-postgres":        true,
		"steadfast-redis":           true,
		"steadfast-worker":          true,
		"steadfast-baseline-worker": true,
	}

	for name := range allowedNames {
		if strings.Contains(nameOrID, name) {
			return nil
		}
	}

	// Verify via docker label steadfast.chaos=allow
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "inspect", "--format", `{{ index .Config.Labels "steadfast.chaos" }}`, nameOrID)
	out, err := cmd.CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == "allow" {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrContainerDenied, nameOrID)
}

func (c *Controller) KillPostgres(ctx context.Context) error {
	if err := c.VerifyAllowed(ctx, "steadfast-postgres"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "kill", "-s", "SIGKILL", "steadfast-postgres")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to kill postgres: %v (output: %s)", err, string(out))
	}
	return nil
}

func (c *Controller) StartPostgres(ctx context.Context) error {
	if err := c.VerifyAllowed(ctx, "steadfast-postgres"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "start", "steadfast-postgres")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start postgres: %v (output: %s)", err, string(out))
	}
	return nil
}

func (c *Controller) KillRedis(ctx context.Context) error {
	if err := c.VerifyAllowed(ctx, "steadfast-redis"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "kill", "-s", "SIGKILL", "steadfast-redis")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to kill redis: %v (output: %s)", err, string(out))
	}
	return nil
}

func (c *Controller) StartRedis(ctx context.Context) error {
	if err := c.VerifyAllowed(ctx, "steadfast-redis"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "start", "steadfast-redis")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start redis: %v (output: %s)", err, string(out))
	}
	return nil
}

func (c *Controller) KillWorkers(ctx context.Context, fraction float64) ([]string, error) {
	if fraction <= 0 {
		fraction = 0.5
	}
	if fraction > 1.0 {
		fraction = 1.0
	}

	// List running containers with label steadfast.chaos=allow or name matching worker
	cmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "ps", "--filter", "status=running", "--format", "{{.ID}}\t{{.Names}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %v (output: %s)", err, string(out))
	}

	var workerIDs []string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) >= 2 {
			id := parts[0]
			name := parts[1]
			if strings.Contains(name, "worker") {
				if err := c.VerifyAllowed(ctx, id); err == nil {
					workerIDs = append(workerIDs, id)
				}
			}
		}
	}

	if len(workerIDs) == 0 {
		return nil, nil
	}

	toKillCount := int(math.Ceil(float64(len(workerIDs)) * fraction))
	if toKillCount > len(workerIDs) {
		toKillCount = len(workerIDs)
	}

	var killed []string
	for i := 0; i < toKillCount; i++ {
		id := workerIDs[i]
		kCmd := exec.CommandContext(ctx, c.cfg.DockerCmd, "kill", "-s", "SIGKILL", id)
		kOut, kErr := kCmd.CombinedOutput()
		if kErr != nil {
			return killed, fmt.Errorf("failed to kill worker %s: %v (output: %s)", id, kErr, string(kOut))
		}
		killed = append(killed, id)
	}

	return killed, nil
}

func (c *Controller) SetLedgerFault(ctx context.Context, mode string, errorRate float64, latencyMs int) error {
	payload := map[string]any{
		"mode": mode,
	}
	if errorRate > 0 {
		payload["error_rate"] = errorRate
	}
	if latencyMs > 0 {
		payload["latency_ms"] = latencyMs
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/admin/fault", strings.TrimRight(c.cfg.LedgerBaseURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if c.cfg.AdminKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AdminKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call ledger fault endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ledger fault returned HTTP %d", resp.StatusCode)
	}

	return nil
}

func (c *Controller) ClearLedgerFaults(ctx context.Context) error {
	return c.SetLedgerFault(ctx, "clear", 0, 0)
}
