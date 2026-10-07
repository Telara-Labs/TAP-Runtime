package main

import (
	"encoding/json"
	"fmt"
	"os"

	"examples.telara.dev/task-summary-go/tap"
)

type task struct {
	ID     *string `json:"id"`
	Title  *string `json:"title"`
	Status *string `json:"status"`
}

func input(args []string) (string, error) {
	if len(args) != 2 {
		return "", fmt.Errorf("expected one JSON input")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
		return "", fmt.Errorf("invalid input JSON: %w", err)
	}
	if len(payload) != 1 || payload["status"] == nil {
		return "", fmt.Errorf("expected only status")
	}
	var status string
	if err := json.Unmarshal(payload["status"], &status); err != nil || (status != "open" && status != "done") {
		return "", fmt.Errorf("expected status open or done")
	}
	return status, nil
}

func summarize(client *tap.Client, status string) (string, error) {
	contents, err := client.Read("examples/languages/fixtures/tasks.json")
	if err != nil {
		return "", err
	}
	var tasks []task
	if err := json.Unmarshal([]byte(contents), &tasks); err != nil || tasks == nil {
		return "", fmt.Errorf("invalid task array")
	}
	items := make([]map[string]string, 0)
	for _, row := range tasks {
		if row.ID == nil || row.Title == nil || row.Status == nil || (*row.Status != "open" && *row.Status != "done") {
			return "", fmt.Errorf("invalid task row")
		}
		if *row.Status == status {
			items = append(items, map[string]string{"id": *row.ID, "title": *row.Title})
		}
	}
	output, err := json.Marshal(map[string]any{"status": status, "count": len(items), "items": items})
	return string(output), err
}

func main() {
	client := tap.New()
	status, err := input(os.Args)
	stdout := ""
	exit := 0
	stderr := ""
	if err == nil {
		stdout, err = summarize(client, status)
	}
	if err != nil {
		exit = 1
		stderr = err.Error()
	}
	if err := client.Return(stdout, stderr, exit); err != nil {
		// A broken protocol stream cannot be repaired with another protocol frame.
		os.Exit(1)
	}
}
