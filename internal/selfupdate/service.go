package selfupdate

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/gosuda/flats/internal/systemd"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func checkService(target, home, goos string) error {
	if home == "" {
		var e error
		home, e = os.UserHomeDir()
		if e != nil {
			return e
		}
	}
	file := systemd.UnitPath(home)
	if goos == "darwin" {
		file = filepath.Join(home, "Library/LaunchAgents/dev.flats.serve.plist")
	}
	b, e := os.ReadFile(file)
	if errors.Is(e, os.ErrNotExist) {
		if _, statErr := os.Lstat(file); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return failure("service_binary", "installed service path cannot be read: %w", e)
	}
	if e != nil {
		return failure("service_binary", "cannot inspect installed service %s: %w", file, e)
	}
	var executable string
	if goos == "darwin" {
		executable, e = plistExecutable(b)
	} else {
		executable, e = unitExecutable(string(b))
	}
	if e != nil || executable == "" {
		return failure("service_binary", "cannot determine executable from installed service %s", file)
	}
	resolved, e := filepath.EvalSymlinks(executable)
	if e != nil {
		return failure("service_binary", "cannot resolve installed service executable: %w", e)
	}
	if resolved == target {
		return failure("service_binary", "executable is used by installed Flats service; use a separate client installation or the service installer")
	}
	return nil
}
func plistExecutable(b []byte) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	want := false
	array := false
	for {
		tok, e := d.Token()
		if e != nil {
			return "", e
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			var key string
			if e = d.DecodeElement(&key, &start); e != nil {
				return "", e
			}
			want = key == "ProgramArguments" || key == "Program"
			array = key == "ProgramArguments"
			continue
		}
		if want && start.Name.Local == "string" {
			var value string
			e = d.DecodeElement(&value, &start)
			return value, e
		}
		if want && array && start.Name.Local == "array" {
			continue
		}
	}
}
func unitExecutable(b string) (string, error) {
	for _, line := range strings.Split(b, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		if value == "" {
			continue
		}
		if value[0] == '"' {
			for i := 1; i < len(value); i++ {
				if value[i] == '\\' {
					i++
					continue
				}
				if value[i] == '"' {
					v, e := strconv.Unquote(value[:i+1])
					return strings.NewReplacer("$$", "$", "%%", "%").Replace(v), e
				}
			}
			return "", fmt.Errorf("unclosed service executable quote")
		}
		parts := strings.Fields(value)
		return parts[0], nil
	}
	return "", io.EOF
}
