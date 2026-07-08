package piko

import (
	"errors"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func IsNullOutput(output string) bool {
	if output == "" {
		return false
	}
	cleaned := filepath.Clean(output)
	if strings.EqualFold(cleaned, filepath.Clean(os.DevNull)) {
		return true
	}
	if strings.EqualFold(cleaned, "NUL:") {
		return true
	}
	return filepath.ToSlash(cleaned) == "/dev/null"
}

func resolveOutputPath(output string, u *url.URL, suggested string) string {
	if output == "" {
		output = suggested
	}
	if output == "" {
		base := path.Base(u.EscapedPath())
		if base == "." || base == "/" || base == "" {
			output = "download.bin"
		} else if decoded, err := url.PathUnescape(base); err == nil && decoded != "" {
			output = decoded
		} else {
			output = base
		}
	}

	if stat, err := os.Stat(output); err == nil && stat.IsDir() {
		name := suggested
		if name == "" {
			name = path.Base(u.Path)
		}
		if name == "." || name == "/" || name == "" {
			name = "download.bin"
		}
		output = filepath.Join(output, filepath.Base(name))
	}
	return output
}

func filenameFromDisposition(value string) string {
	if value == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	if name := params["filename"]; name != "" {
		return filepath.Base(name)
	}
	return ""
}

func prepareOutput(output string, force bool) error {
	dir := filepath.Dir(output)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(output); err == nil && !force {
		return &OutputExistsError{Path: output}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func prepareTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func finishOutput(partPath, output string, force bool) error {
	if force {
		if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(partPath, output)
}

type OutputExistsError struct {
	Path string
}

func (e *OutputExistsError) Error() string {
	return e.Path + " already exists; use -f to overwrite"
}
