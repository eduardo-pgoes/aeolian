package host

import (
	"io"
	"os"
	"strings"
)

const logTailLines = 30

func tailLog(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const window = 256 << 10
	cut := fi.Size() > window
	if cut {
		if _, err := f.Seek(-window, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if cut {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	return lines[max(0, len(lines)-n):], nil
}
