// Package localchat writes commentary scripts with a model on this machine.
//
// The booth's scripts normally come from Gemini, which is the right default: it
// is the better writer, and a script is a few hundred tokens a minute. But a
// race day is exactly where that assumption breaks — venue internet is
// frequently the worst part of the setup, and a booth that goes silent when the
// uplink drops is a booth nobody trusts to run a meeting.
//
// So this is the local substitute. It drives the same sidecar as the local
// voice, in a mode that loads ONLY the chat model, and satisfies the same
// scriptWriter seam the cloud clients do — the booth builds its prompt exactly
// as before and neither knows nor cares which answered.
//
// It runs as its OWN process rather than sharing the voice's sidecar. Writing a
// script takes seconds and rendering a lap call takes a fraction of one; on a
// shared pipe the call would queue behind the commentary, which is precisely
// the contention the whole audio design exists to remove. Two processes make
// that impossible rather than merely unlikely.
package localchat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sidecarLog surfaces the sidecar's own progress and errors. A model that will
// not load is the failure most worth seeing, and it reports on stderr.
var sidecarLog = func(msg string) { log.Printf("local script: %s", msg) }

// Client generates text using a local model.
type Client struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan string

	model     string
	ready     bool
	timeout   time.Duration
	maxTokens int
}

// Config describes how to start the sidecar.
type Config struct {
	// Python with mlx-lm installed.
	Python string

	// The sidecar script — the same one the local voice uses, started in its
	// chat-only mode.
	Script string

	// Model to write with.
	Model string

	// StartTimeout bounds waiting for the model to load.
	//
	// Generous by default: a 30B model takes minutes on a first run while the
	// weights download, and giving up on it would leave the booth mute for the
	// meeting it was started for.
	StartTimeout time.Duration

	// Timeout bounds one script.
	Timeout time.Duration

	// MaxTokens caps a script's length.
	MaxTokens int
}

func (c Config) withDefaults() Config {
	if c.StartTimeout == 0 {
		c.StartTimeout = 10 * time.Minute
	}
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 320
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	return c
}

// DefaultModel is a mixture-of-experts model: 30B of knowledge with only ~3B
// active per token, so it writes with far more range than a 3B dense model at
// close to the same speed. That trade is what makes a local booth listenable
// rather than merely possible.
const DefaultModel = "mlx-community/Qwen3-30B-A3B-4bit"

// Start launches the sidecar and waits for the model.
//
// An error is never fatal to a meeting: the caller keeps whatever writer it had,
// or the booth stays quiet. Losing local scripts costs colour, never the race
// calls.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()

	if cfg.Python == "" || cfg.Script == "" {
		return nil, fmt.Errorf("local scripts need a python and a script path")
	}

	cmd := exec.Command(cfg.Python, cfg.Script, "--llm-only", "--llm", cfg.Model)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("local script stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("local script stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("local script stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start local script model: %w", err)
	}

	c := &Client{
		cmd:       cmd,
		stdin:     stdin,
		replies:   make(chan string, 16),
		model:     cfg.Model,
		timeout:   cfg.Timeout,
		maxTokens: cfg.MaxTokens,
	}

	// One reader draining into a channel. Reading on demand would leave a
	// timed-out read still blocked on the pipe and swallowing the NEXT reply,
	// which desynchronises it permanently — every later answer belongs to the
	// previous request.
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for sc.Scan() {
			select {
			case c.replies <- sc.Text():
			default: // nobody waiting; dropping is correct
			}
		}
		close(c.replies)
	}()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "#")); line != "" {
				sidecarLog(line)
			}
		}
	}()

	if _, ok := c.waitFor("READY", cfg.StartTimeout); !ok {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("local script model never became ready within %s", cfg.StartTimeout)
	}

	c.ready = true
	return c, nil
}

// GenerateText writes from a prompt, satisfying the same seam as the cloud
// clients.
func (c *Client) GenerateText(ctx context.Context, prompt string) (string, error) {
	if c == nil || !c.ready {
		return "", fmt.Errorf("local script model not running")
	}

	// The prompt is passed through EXACTLY as built. The booth's prompt already
	// carries the format, the beat brief, the style and its own don't-repeat
	// transcript; adding to it here would put two sets of instructions in front
	// of the model and let the local path drift from the cloud one it stands in
	// for.
	req := fmt.Sprintf(`{"prompt":%s,"max_tokens":%d}`, jsonString(prompt), c.maxTokens)

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := fmt.Fprintf(c.stdin, "WRITE %s\n", req); err != nil {
		c.ready = false
		return "", fmt.Errorf("local script write: %w", err)
	}

	timeout := c.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	reply, ok := c.waitFor("WROTE", timeout)
	if !ok {
		return "", fmt.Errorf("local script model timed out after %s", timeout)
	}

	out := strings.TrimSpace(reply)
	if out == "" {
		return "", fmt.Errorf("local script model returned nothing")
	}
	return out, nil
}

// Model reports which model is writing, for logging.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// Close stops the sidecar.
func (c *Client) Close() error {
	if c == nil || c.cmd == nil {
		return nil
	}
	c.ready = false

	c.mu.Lock()
	_, _ = fmt.Fprintln(c.stdin, "QUIT")
	_ = c.stdin.Close()
	c.mu.Unlock()

	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Kill()
	}
	return nil
}

func (c *Client) waitFor(prefix string, timeout time.Duration) (string, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case line, open := <-c.replies:
			if !open {
				return "", false // the sidecar exited
			}
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix), true
			}
		case <-deadline:
			return "", false
		}
	}
}

// jsonString quotes a prompt for the sidecar's one-line protocol.
//
// Hand-written rather than encoding/json because the requirement is narrow and
// absolute: the result must contain no raw newline. The protocol is line-based,
// and a prompt spanning lines would be read as several commands — with the tail
// of the prompt interpreted as one.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
