package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	port      = flag.String("port", "9155", "Port to listen on (leave empty to disable TCP)")
	socket    = flag.String("socket", "", "Unix socket path to listen on (e.g. /tmp/timemachine.sock)")
	plistPath = "/Library/Preferences/com.apple.TimeMachine.plist"
	logger    = slog.New(slog.NewTextHandler(os.Stderr, nil))
)

// Helper to run tmutil destinationinfo and parse
type TmDestination struct {
	Name string
	ID   string
	Kind string
}

func getDestinations() ([]TmDestination, error) {
	cmd := exec.Command("tmutil", "destinationinfo")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	var dests []TmDestination
	var current TmDestination

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name") {
			if current.Name != "" {
				dests = append(dests, current)
			}
			current = TmDestination{}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				current.Name = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "Kind") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				current.Kind = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "ID") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				current.ID = strings.TrimSpace(parts[1])
			}
		}
	}
	if current.Name != "" {
		dests = append(dests, current)
	}

	return dests, nil
}

func getLatestBackupFromTmutil() (int64, error) {
	cmd := exec.Command("tmutil", "latestbackup")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	path := strings.TrimSpace(string(out))
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		if strings.HasSuffix(part, ".backup") {
			part = strings.TrimSuffix(part, ".backup")
		}
		// Time Machine folder names are in local time
		t, err := time.ParseInLocation("2006-01-02-150405", part, time.Local)
		if err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("no date found in path")
}

func getLatestBackupFromPlist() (int64, error) {
	// Uses plutil to convert binary plist to JSON for easy parsing
	cmd := exec.Command("plutil", "-extract", "Destinations", "json", "-o", "-", plistPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	var dests []map[string]interface{}
	if err := json.Unmarshal(out, &dests); err != nil {
		return 0, err
	}

	var maxTime int64
	for _, d := range dests {
		if snaps, ok := d["SnapshotDates"].([]interface{}); ok {
			for _, s := range snaps {
				if dateStr, ok := s.(string); ok {
					t, err := time.Parse(time.RFC3339, dateStr)
					if err == nil {
						if t.Unix() > maxTime {
							maxTime = t.Unix()
						}
					}
				}
			}
		}
	}

	if maxTime > 0 {
		return maxTime, nil
	}
	return 0, fmt.Errorf("no snapshots found in plist")
}

func getLatestBackup() (int64, error) {
	ts, err := getLatestBackupFromTmutil()
	if err == nil && ts > 0 {
		return ts, nil
	}
	return getLatestBackupFromPlist()
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	ts, err := getLatestBackup()
	if err != nil {
		logger.Error("Failed to get latest backup", "error", err)
	}

	running := 0
	statusOut, _ := exec.Command("tmutil", "status").Output()
	if strings.Contains(string(statusOut), "Running = 1") {
		running = 1
	}

	dests, err := getDestinations()
	if err == nil && len(dests) > 0 {
		d := dests[0]
		fmt.Fprintln(w, "# HELP timemachine_destination_info Apple Time Machine Backup current destination info")
		fmt.Fprintln(w, "# TYPE timemachine_destination_info gauge")
		fmt.Fprintf(w, "timemachine_destination_info{volume=\"%s\",kind=\"%s\",id=\"%s\"} 1\n", d.Name, d.Kind, d.ID)
	}

	if ts > 0 {
		age := time.Now().Unix() - ts

		fmt.Fprintln(w, "# HELP timemachine_last_success_timestamp_seconds Unix timestamp of the last successful backup.")
		fmt.Fprintln(w, "# TYPE timemachine_last_success_timestamp_seconds gauge")
		fmt.Fprintln(w, "timemachine_last_success_timestamp_seconds", ts)

		fmt.Fprintln(w, "# HELP timemachine_destination_latest_age_seconds Seconds since the last successful Apple Time Machine Backup")
		fmt.Fprintln(w, "# TYPE timemachine_destination_latest_age_seconds gauge")
		fmt.Fprintln(w, "timemachine_destination_latest_age_seconds", age)
	}

	fmt.Fprintln(w, "# HELP timemachine_backup_running 1 if backup is in progress, 0 otherwise.")
	fmt.Fprintln(w, "# TYPE timemachine_backup_running gauge")
	fmt.Fprintln(w, "timemachine_backup_running", running)
}

func main() {
	flag.Parse()
	http.HandleFunc("/metrics", metricsHandler)

	if *port == "" && *socket == "" {
		logger.Error("Either port or socket must be specified")
		os.Exit(1)
	}

	errChan := make(chan error, 2)

	if *port != "" {
		go func() {
			logger.Info("Starting Time Machine exporter on TCP", "port", *port)
			errChan <- http.ListenAndServe(":"+*port, nil)
		}()
	}

	if *socket != "" {
		go func() {
			logger.Info("Starting Time Machine exporter on Unix socket", "socket", *socket)
			if err := os.Remove(*socket); err != nil && !os.IsNotExist(err) {
				errChan <- fmt.Errorf("failed to remove existing socket: %w", err)
				return
			}
			listener, err := net.Listen("unix", *socket)
			if err != nil {
				errChan <- err
				return
			}
			server := &http.Server{Handler: nil}
			errChan <- server.Serve(listener)
		}()
	}

	err := <-errChan
	logger.Error("Server failed", "error", err)
	os.Exit(1)
}
