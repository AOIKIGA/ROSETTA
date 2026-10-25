//go:build darwin || linux

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fileTools(t *testing.T) (string, ReadFiles, ListFiles) {
	t.Helper()
	dir := t.TempDir()
	w, err := OpenWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return dir, ReadFiles{w}, ListFiles{w}
}

func TestReadFilesRangesErrorsAndBounds(t *testing.T) {
	dir, read, _ := fileTools(t)
	for name, data := range map[string]string{"note.txt": "one\n二\nthree\nfour\n", "huge.txt": strings.Repeat("a", maxFileBytes+1), "binary": "abc\x00xyz", "quotes.txt": strings.Repeat("\"\t中文\n", 500)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := read.Execute(context.Background(), `{"paths":["note.txt"],"start_line":2,"max_lines":2}`)
	var response struct {
		Files []fileRead `json:"files"`
	}
	if err != nil || result.IsError || json.Unmarshal([]byte(result.Content), &response) != nil {
		t.Fatalf("%+v %v", result, err)
	}
	if f := response.Files[0]; f.Content != "二\nthree\n" || f.EndLine != 3 || !f.Truncated {
		t.Fatalf("%+v", f)
	}
	result, err = read.Execute(context.Background(), `{"paths":["note.txt","missing","huge.txt","binary"]}`)
	if err != nil || !result.IsError || json.Unmarshal([]byte(result.Content), &response) != nil {
		t.Fatalf("%+v %v", result, err)
	}
	for i, code := range []string{"", "file_unavailable", "file_too_large", "not_utf8_text"} {
		if response.Files[i].Error != code {
			t.Fatalf("%+v", response.Files)
		}
	}
	result, err = read.Execute(context.Background(), `{"paths":["quotes.txt","quotes.txt","quotes.txt","quotes.txt"],"max_lines":200}`)
	if err != nil || !result.Truncated || len(WireContent(result)) > MaxOutputBytes {
		t.Fatalf("%+v %v", result, err)
	}
	var wire Result
	if json.Unmarshal([]byte(WireContent(result)), &wire) != nil || json.Unmarshal([]byte(wire.Content), &response) != nil {
		t.Fatal("structured result was chopped")
	}
	for _, args := range []string{`null`, `{}`, `{"paths":null}`, `{"paths":[null]}`, `{"paths":["note.txt"],"max_lines":null}`, `{"paths":["note.txt"],"start_line":0}`, `{"paths":["note.txt"],"extra":1}`, `{"paths":["note.txt"]} {}`, `{"paths":["../outside"]}`, `{"paths":["/etc/passwd"]}`} {
		result, err := read.Execute(context.Background(), args)
		if err != nil || !result.IsError {
			t.Fatalf("accepted %s: %+v %v", args, result, err)
		}
	}
}

func TestWorkspaceEscapeAndSpecialFileProtection(t *testing.T) {
	dir, read, list := fileTools(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"escape/secret", "link", "pipe", "."} {
		started := time.Now()
		args, _ := json.Marshal(map[string]any{"paths": []string{path}})
		result, err := read.Execute(context.Background(), string(args))
		if err != nil || !result.IsError || strings.Contains(result.Content, "outside-sentinel") {
			t.Fatalf("%s: %+v %v", path, result, err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("special file blocked")
		}
	}
	if result, err := list.Execute(context.Background(), `{"path":"escape"}`); err != nil || !result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := read.Execute(ctx, `{"paths":["link"]}`); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestListFilesIsBoundedAndNonrecursive(t *testing.T) {
	dir, _, list := fileTools(t)
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "folder", "nested"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 110 {
		f, err := os.CreateTemp(dir, strings.Repeat("x", 100))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	result, err := list.Execute(context.Background(), `{}`)
	if err != nil || result.IsError || !result.Truncated || !fitsWire(result) {
		t.Fatalf("%+v %v", result, err)
	}
	if !json.Valid([]byte(result.Content)) || strings.Contains(result.Content, "nested") {
		t.Fatal("invalid/recursive listing")
	}
}
