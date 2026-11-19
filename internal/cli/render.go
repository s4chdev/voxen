package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/s4chdev/voxen/internal/config"
)

// absPath resolves p against the working directory.
func absPath(p string) (string, error) {
	return filepath.Abs(p)
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints an aligned list of rows. Human output only: --json is the stable
// machine interface.
type table struct {
	headers []string
	rows    [][]string
}

func (t *table) add(cols ...string) { t.rows = append(t.rows, cols) }

func (a *App) render(t *table) {
	if len(t.rows) == 0 {
		return
	}
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = len(h)
	}
	for _, row := range t.rows {
		for len(widths) < len(row) {
			widths = append(widths, 0)
		}
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	line := func(cols []string) {
		for i, c := range cols {
			if i == len(cols)-1 {
				fmt.Fprintln(a.stdout, c)
				continue
			}
			fmt.Fprintf(a.stdout, "%-*s  ", widths[i], c)
		}
	}
	if len(t.headers) > 0 {
		line(t.headers)
	}
	for _, row := range t.rows {
		line(row)
	}
}

// loadLocalConfig reads the configuration from disk for commands that need it
// before (or without) a daemon.
func (a *App) loadLocalConfig() error {
	cfg, err := config.Load(a.paths.ConfigFile)
	if err != nil {
		return err
	}
	a.cfg = cfg
	return nil
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
