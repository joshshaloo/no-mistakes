package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Notice traffic is separate from description readback: posting a comment must
// neither overwrite the PR body nor masquerade as its last --body argument.
func fakeNoticeCommands(args []string) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") != "gh" || len(args) < 2 {
		return
	}
	path := os.Getenv("FAKE_CLI_RISK_BODY")
	if args[0] == "pr" && args[1] == "comment" {
		if os.Getenv("FAKE_CLI_RISK_EDIT_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "notice publication unavailable")
			os.Exit(1)
		}
		body, err := readFakeBody()
		if err == nil && path != "" {
			err = os.WriteFile(path, body, 0o644)
			if err == nil {
				var f *os.File
				f, err = os.OpenFile(path+".jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err == nil {
					err = json.NewEncoder(f).Encode(map[string]string{"body": string(body)})
					if closeErr := f.Close(); err == nil {
						err = closeErr
					}
				}
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if args[0] != "api" || !strings.Contains(strings.Join(args, " "), "/issues/") || !strings.Contains(strings.Join(args, " "), "/comments") {
		return
	}
	if os.Getenv("FAKE_CLI_RISK_LOOKUP_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "notice lookup unavailable")
		os.Exit(1)
	}
	comments := []json.RawMessage{}
	if path != "" {
		f, err := os.Open(path + ".jsonl")
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if f != nil {
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				comments = append(comments, append(json.RawMessage(nil), scanner.Bytes()...))
			}
			err = scanner.Err()
			_ = f.Close()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	fmt.Print("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
	if err := json.NewEncoder(os.Stdout).Encode(comments); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
