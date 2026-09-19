package localexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/procexec"
)

// FileInfo describes one directory entry. The JSON shape matches
// sandbox.FileInfo so a client reading either server sees the same fields.
//
// Declared here rather than shared with internal/sandbox because it is the only
// thing the two would share: importing the sandbox for one data type would drag
// its event bus, its os.Root confinement and its manager into a binary that
// deliberately has none of them.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// DeleteResult describes what a delete actually removed. RemoveAll succeeds on a
// path that was never there, so without this a caller cannot tell "deleted" from
// "there was nothing to delete", nor one file from a whole subtree.
type DeleteResult struct {
	Existed      bool `json:"existed"`
	WasDirectory bool `json:"was_directory"`
}

// Status reports how the runner is configured.
type Status struct {
	RootDir        string `json:"root_dir"`
	Shell          string `json:"shell"`
	DefaultTimeout string `json:"default_timeout"`
	MaxOutputBytes int    `json:"max_output_bytes"`

	// Stated in the tool output, not only in the README: an agent that believes it
	// is sandboxed will take risks it would not otherwise take.
	Sandboxed bool `json:"sandboxed"`
}

// resolvePath turns a caller-supplied path into an absolute one.
//
// A relative path resolves against the configured directory; an absolute path is
// taken as given. Neither is checked for containment — there is nothing here to
// contain it to, and a check that the same agent could step around with one `cd`
// would be a gesture rather than a boundary. See the package comment.
func (r *Runner) resolvePath(path string) string {
	if path == "" || path == "." {
		return r.cfg.Dir
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(r.cfg.Dir, path)
}

// ReadFile returns a file's contents, bounded by the output cap.
//
// Reads are capped for the same reason command output is: the contents travel
// back inside one MCP response, and a caller that asks for a multi-gigabyte file
// should get a truncated answer rather than exhaust this process's memory. The
// flag says which happened.
func (r *Runner) ReadFile(path string) (content string, truncated bool, err error) {
	absPath := r.resolvePath(path)

	file, err := os.Open(absPath) // #nosec G304 -- an unconfined read is this helper's contract; see the package comment
	if err != nil {
		return "", false, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", absPath)
	}

	buf := procexec.NewCappedBuffer(r.cfg.MaxOutputBytes)
	// One byte past the cap, because the buffer only learns it truncated once a
	// write exceeds the limit — reading exactly the cap would report a file of
	// exactly that size as complete when it might not be.
	if _, err := io.Copy(buf, io.LimitReader(file, int64(r.cfg.MaxOutputBytes)+1)); err != nil {
		return "", false, err
	}
	return buf.String(), buf.Truncated(), nil
}

// WriteFile writes data to path, creating parent directories.
func (r *Runner) WriteFile(path string, data []byte) error {
	absPath := r.resolvePath(path)
	if dir := filepath.Dir(absPath); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create directory structure: %w", err)
		}
	}
	return os.WriteFile(absPath, data, 0o644) // #nosec G306 -- ordinary file permissions for files the operator's own tools will read
}

// ListDir lists a directory, non-recursively.
func (r *Runner) ListDir(path string) ([]FileInfo, error) {
	absPath := r.resolvePath(path)

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, err
	}

	results := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// Removed between ReadDir and Info; it is simply no longer part of the
			// listing.
			continue
		}
		// Reported relative to the configured directory when it is inside it, so
		// the paths can be handed straight back to the other tools.
		reported := entry.Name()
		if rel, relErr := filepath.Rel(r.cfg.Dir, filepath.Join(absPath, entry.Name())); relErr == nil {
			reported = rel
		}
		results = append(results, FileInfo{
			Name:    entry.Name(),
			Path:    reported,
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return results, nil
}

// DeleteFile removes a file, or a directory and its subtree.
//
// Two targets are refused: the filesystem root and the configured directory
// itself. Neither is confinement — an agent can still delete anything else the
// operator can — but both are almost certainly a mistake rather than an
// instruction, and the cost of being wrong about them is total.
func (r *Runner) DeleteFile(path string) (DeleteResult, error) {
	absPath := r.resolvePath(path)

	if absPath == filepath.Dir(absPath) {
		return DeleteResult{}, errors.New("refusing to delete the filesystem root")
	}
	if absPath == r.cfg.Dir {
		return DeleteResult{}, errors.New("refusing to delete the working directory itself")
	}

	var result DeleteResult
	// Lstat, not Stat: a symlink is removed as a link, so what matters is what the
	// entry is rather than what it points at.
	if info, statErr := os.Lstat(absPath); statErr == nil {
		result.Existed = true
		result.WasDirectory = info.IsDir()
	}

	if err := os.RemoveAll(absPath); err != nil {
		return DeleteResult{}, err
	}
	return result, nil
}

// Status returns the runner's configuration.
func (r *Runner) Status() Status {
	return Status{
		RootDir:        r.cfg.Dir,
		Shell:          r.cfg.Shell,
		DefaultTimeout: r.cfg.DefaultTimeout.String(),
		MaxOutputBytes: r.cfg.MaxOutputBytes,
		Sandboxed:      false,
	}
}
