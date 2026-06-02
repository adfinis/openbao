package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Run struct {
	ID  string
	Dir string

	logMu sync.Mutex
}

func New(root, prefix string) (*Run, error) {
	id := fmt.Sprintf("%s-%s", prefix, time.Now().UTC().Format("20060102T150405Z"))
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		return nil, fmt.Errorf("create segment dir: %w", err)
	}
	return &Run{ID: id, Dir: dir}, nil
}

func (r *Run) Path(parts ...string) string {
	all := append([]string{r.Dir}, parts...)
	return filepath.Join(all...)
}

func (r *Run) WriteJSON(name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", name, err)
	}
	raw = append(raw, '\n')
	return r.WriteFile(name, raw)
}

func (r *Run) WriteFile(name string, data []byte) error {
	path := r.Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (r *Run) AppendLog(name, line string) error {
	r.logMu.Lock()
	defer r.logMu.Unlock()

	path := r.Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func (r *Run) Logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line)
	_ = r.AppendLog("orchestrator.log", line)
}
