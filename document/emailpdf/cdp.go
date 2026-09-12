package emailpdf

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

type cdpClient struct {
	reader        *bufio.Reader
	writer        io.Writer
	id            int
	loadedLoader  string
	loadedSession string
}
type cdpMessage struct {
	ID        int            `json:"id"`
	Method    string         `json:"method"`
	Result    jsontext.Value `json:"result"`
	Error     jsontext.Value `json:"error"`
	SessionID string         `json:"sessionId"`
	Params    jsontext.Value `json:"params"`
}

func newCDP(r io.Reader, w io.Writer) *cdpClient {
	return &cdpClient{reader: bufio.NewReaderSize(r, 64<<10), writer: w}
}
func (c *cdpClient) read(ctx context.Context) (cdpMessage, error) {
	var b []byte
	for {
		if err := ctx.Err(); err != nil {
			return cdpMessage{}, err
		}
		part, err := c.reader.ReadSlice(0)
		if len(b)+len(part) > 32<<20 {
			return cdpMessage{}, errors.New("chromium DevTools response exceeds limit")
		}
		b = append(b, part...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return cdpMessage{}, fmt.Errorf("reading Chromium DevTools frame: %w", err)
		}
	}
	var message cdpMessage
	if err := json.Unmarshal(b[:len(b)-1], &message); err != nil {
		return message, errors.New("chromium DevTools response is malformed")
	}
	if message.Method == "Page.lifecycleEvent" {
		var lifecycle struct {
			LoaderID string `json:"loaderId"`
			Name     string `json:"name"`
		}
		if err := json.Unmarshal(message.Params, &lifecycle); err != nil {
			return message, errors.New("chromium lifecycle event is malformed")
		}
		if lifecycle.Name == "load" {
			c.loadedLoader, c.loadedSession = lifecycle.LoaderID, message.SessionID
		}
	}
	return message, nil
}
func (c *cdpClient) call(ctx context.Context, method string, params any, out any, session string) error {
	c.id++
	request := struct {
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
		Session string `json:"sessionId,omitempty"`
	}{c.id, method, params, session}
	b, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err = c.writer.Write(append(b, 0)); err != nil {
		return fmt.Errorf("chromium DevTools %s write: %w", method, err)
	}
	for {
		m, err := c.read(ctx)
		if err != nil {
			return fmt.Errorf("chromium DevTools %s response: %w", method, err)
		}
		if m.ID == 0 {
			continue
		}
		if m.ID != c.id {
			return errors.New("chromium DevTools response identity mismatch")
		}
		if len(m.Error) != 0 {
			return fmt.Errorf("chromium DevTools %s was rejected", method)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	}
}

// RunWorker handles only an already staged HTML file. It is called inside
// the isolated transient unit by docbank's hidden worker entrypoint, never
// opens a vault, and exposes no network listener.
func RunWorker(ctx context.Context, chromium, dir, expectedVersion string) error {
	if !filepath.IsAbs(chromium) || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("invalid email PDF worker paths")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return errors.New("email PDF worker staging cannot be a symlink")
	}
	input := filepath.Join(dir, "message.html")
	info, err := os.Lstat(input)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxHTMLBytes {
		return errors.New("email PDF worker requires bounded staged HTML")
	}
	readBrowser, writeClient, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = readBrowser.Close(); _ = writeClient.Close() }()
	readClient, writeBrowser, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = readClient.Close(); _ = writeBrowser.Close() }()
	args := []string{"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions", "--disable-default-apps", "--disable-breakpad", "--disable-crash-reporter", "--remote-debugging-pipe", "--user-data-dir=" + filepath.Join(dir, "profile"), "about:blank"}
	cmd := exec.CommandContext(ctx, chromium, args...) //nolint:gosec // Exact pinned renderer path supplied by the isolated parent.
	cmd.ExtraFiles = []*os.File{readBrowser, writeBrowser}
	cmd.Env = os.Environ()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("starting isolated Chromium: %w", err)
	}
	_ = readBrowser.Close()
	_ = writeBrowser.Close()
	stop := context.AfterFunc(ctx, func() { _ = readClient.Close(); _ = writeClient.Close() })
	defer stop()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	c := newCDP(readClient, writeClient)
	var version struct {
		Product string `json:"product"`
	}
	if err = c.call(ctx, "Browser.getVersion", nil, &version, ""); err != nil {
		return err
	}
	if version.Product != "Chrome/"+expectedVersion {
		return errors.New("unexpected Chromium DevTools product")
	}
	// A private readiness witness lets cancellation qualification observe a real
	// connected Chromium process, rather than racing an arbitrary timer.
	if err = os.WriteFile(filepath.Join(dir, "renderer-ready"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		return err
	}
	var target struct {
		ID string `json:"targetId"`
	}
	if err = c.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, &target, ""); err != nil {
		return err
	}
	var attached struct {
		ID string `json:"sessionId"`
	}
	if err = c.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached, ""); err != nil {
		return err
	}
	for _, request := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil}, {"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}}, {"Network.enable", nil},
		{"Network.setBlockedURLs", map[string]any{"urls": []string{"http://*", "https://*", "ws://*", "wss://*", "ftp://*"}}},
		{"Network.setBypassServiceWorker", map[string]any{"bypass": true}},
		{"Emulation.setScriptExecutionDisabled", map[string]any{"value": true}},
		{"Emulation.setTimezoneOverride", map[string]any{"timezoneId": "UTC"}},
		{"Emulation.setLocaleOverride", map[string]any{"locale": "en-US"}},
	} {
		if err = c.call(ctx, request.method, request.params, nil, attached.ID); err != nil {
			return err
		}
	}
	c.loadedLoader = ""
	var navigation struct {
		ErrorText string `json:"errorText"`
		LoaderID  string `json:"loaderId"`
	}
	if err = c.call(ctx, "Page.navigate", map[string]any{"url": "file://" + input}, &navigation, attached.ID); err != nil {
		return err
	}
	if navigation.ErrorText != "" || navigation.LoaderID == "" {
		return errors.New("chromium could not navigate staged email")
	}
	for c.loadedLoader != navigation.LoaderID || c.loadedSession != attached.ID {
		if _, err = c.read(ctx); err != nil {
			return fmt.Errorf("chromium email load event: %w", err)
		}
	}
	var printed struct {
		Stream string `json:"stream"`
	}
	if err = c.call(ctx, "Page.printToPDF", map[string]any{"printBackground": true, "preferCSSPageSize": true, "displayHeaderFooter": false, "transferMode": "ReturnAsStream"}, &printed, attached.ID); err != nil {
		return err
	}
	if printed.Stream == "" {
		return errors.New("chromium returned no PDF stream")
	}
	f, err := os.OpenFile(filepath.Join(dir, "message.pdf"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var total int
	for {
		var chunk struct {
			Data   string `json:"data"`
			Base64 bool   `json:"base64Encoded"`
			EOF    bool   `json:"eof"`
		}
		if err = c.call(ctx, "IO.read", map[string]any{"handle": printed.Stream, "size": 1 << 20}, &chunk, attached.ID); err != nil {
			return err
		}
		b := []byte(chunk.Data)
		if chunk.Base64 {
			b, err = base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return errors.New("chromium PDF stream encoding invalid")
			}
		}
		if len(b) > MaxPDFBytes-total {
			return errors.New("chromium PDF output exceeds limit")
		}
		total += len(b)
		if _, err = f.Write(b); err != nil {
			return err
		}
		if chunk.EOF {
			break
		}
		if len(b) == 0 {
			return errors.New("chromium PDF stream made no progress")
		}
	}
	if total == 0 {
		return errors.New("chromium PDF stream is empty")
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = c.call(ctx, "IO.close", map[string]any{"handle": printed.Stream}, nil, attached.ID); err != nil {
		return err
	}
	// Exercise the independent parser inside the same memory/time boundary
	// before reporting worker success. The parent rechecks normalized bytes.
	proof, err := os.Open(filepath.Join(dir, "message.pdf"))
	if err != nil {
		return err
	}
	pdf, readErr := io.ReadAll(io.LimitReader(proof, MaxPDFBytes+1))
	if err = errors.Join(readErr, proof.Close()); err != nil {
		return err
	}
	if _, err = VerifyPDF(pdf); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return c.call(ctx, "Browser.close", nil, nil, "")
}
