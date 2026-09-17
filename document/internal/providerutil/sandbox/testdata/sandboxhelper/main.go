package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	mode           = "echo"
	networkAddress string
	outputName     = "output.bin"
)

type echoResponse struct {
	Arguments   []string `json:"arguments"`
	Environment []string `json:"environment"`
	StdinSHA256 string   `json:"stdin_sha256"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		if mode == "file-descendant-exit" {
			if err := os.WriteFile("/work/descendant-ready", []byte("ready"), 0o600); err != nil {
				os.Exit(6)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
		if _, err := fmt.Fprintln(os.Stdout, "descendant-ready"); err != nil || os.Stdout.Sync() != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	switch mode {
	case "echo", "replacement":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		digest := sha256.Sum256(data)
		_ = json.NewEncoder(os.Stdout).Encode(echoResponse{
			Arguments: os.Args[1:], Environment: os.Environ(),
			StdinSHA256: hex.EncodeToString(digest[:]),
		})
	case "network":
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "strict-fs":
		probePath := "/tmp/docbank-strict-probe"
		writeErr := os.WriteFile(probePath, []byte("temp"), 0o600)
		data, readErr := os.ReadFile(probePath)
		removeErr := os.Remove(probePath)
		comm, commErr := os.ReadFile("/proc/1/comm")
		privateProc := commErr == nil && !strings.Contains(strings.ToLower(string(comm)), "init")
		tmpAllowed := writeErr == nil && readErr == nil && string(data) == "temp" && removeErr == nil
		fmt.Printf("proc=%t;tmp=%t;comm=%s;write=%v;read=%v;remove=%v", privateProc, tmpAllowed, strings.TrimSpace(string(comm)), writeErr, readErr, removeErr)
	case "file-network":
		status := "denied"
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err == nil {
			status = "connected"
			_ = connection.Close()
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-unix-network":
		status := "denied"
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err == nil {
			status = "connected"
			_ = connection.Close()
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "local-ipc":
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			os.Exit(7)
		}
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
		if err := os.WriteFile("/work/"+outputName, []byte("allowed"), 0o600); err != nil {
			os.Exit(5)
		}
	case "unix-network":
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "host-file":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		fmt.Print(status)
	case "file-host":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "descendant":
		command := exec.Command("/proc/self/exe", "--descendant")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Print("spawned")
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	case "overflow":
		block := make([]byte, 32<<10)
		for {
			if _, err := os.Stdout.Write(block); err != nil {
				return
			}
		}
	case "file-output":
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-overflow":
		if err := os.WriteFile("/work/"+outputName, make([]byte, 2048), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-descendant-exit":
		command := exec.Command("/proc/self/exe", "--descendant")
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		if err := waitForFile("/work/descendant-ready"); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-exit81-reconstruct":
		if err := runReconstructedRetry(); err != nil {
			os.Exit(6)
		}
	case "file-exit81-no-output":
		if retryMarkerMissing() {
			if err := os.WriteFile("/work/"+outputName, []byte("stale output"), 0o600); err != nil {
				os.Exit(5)
			}
			if err := markRetry(); err != nil {
				os.Exit(5)
			}
			os.Exit(81)
		}
	case "file-exit81-hardlink":
		if retryMarkerMissing() {
			if err := os.WriteFile("/work/"+outputName, []byte("stale output"), 0o600); err != nil {
				os.Exit(5)
			}
			if err := os.Link("/work/"+outputName, "/work/stale-output-link"); err != nil {
				os.Exit(5)
			}
			if err := markRetry(); err != nil {
				os.Exit(5)
			}
			os.Exit(81)
		}
		if err := os.Rename("/work/stale-output-link", "/work/"+outputName); err != nil {
			os.Exit(6)
		}
	case "file-exit81-profile-symlink":
		if profileParentRetryMarkerMissing() {
			if err := markProfileParentRetry(); err != nil {
				os.Exit(5)
			}
			if err := os.MkdirAll("/work/home/user", 0o700); err != nil {
				os.Exit(5)
			}
			if err := os.RemoveAll("/work/profile"); err != nil {
				os.Exit(5)
			}
			if err := os.Symlink("/work/home", "/work/profile"); err != nil {
				os.Exit(5)
			}
			os.Exit(81)
		}
		if err := writeRetryOutput(); err != nil {
			os.Exit(6)
		}
	case "file-exit81-user-symlink":
		if profileParentRetryMarkerMissing() {
			if err := markProfileParentRetry(); err != nil {
				os.Exit(5)
			}
			if err := os.RemoveAll("/work/profile/user"); err != nil {
				os.Exit(5)
			}
			if err := os.Symlink("/work/home", "/work/profile/user"); err != nil {
				os.Exit(5)
			}
			os.Exit(81)
		}
		if err := writeRetryOutput(); err != nil {
			os.Exit(6)
		}
	case "argv":
		if err := os.WriteFile("/work/"+outputName, []byte(strings.Join(os.Args[1:], "\x00")), 0o600); err != nil {
			os.Exit(5)
		}
	case "runtime-mounts":
		if len(os.Args) < 4 {
			os.Exit(6)
		}
		statuses := make([]string, 0, len(os.Args)-2)
		for _, path := range os.Args[2:] {
			var stat unix.Statfs_t
			if err := unix.Statfs(path, &stat); err != nil {
				os.Exit(7)
			}
			statuses = append(statuses, filepath.Base(path)+"="+strconv.FormatBool(stat.Flags&unix.ST_NOEXEC == 0))
		}
		execBytes, err := os.ReadFile("/usr/runtime/exec")
		if err != nil {
			os.Exit(8)
		}
		dataBytes, err := os.ReadFile("/usr/runtime/data")
		if err != nil {
			os.Exit(8)
		}
		payload := strings.Join(statuses, ",") + ";exec-content=" + string(execBytes) + ";data-content=" + string(dataBytes)
		if err := os.WriteFile("/work/"+outputName, []byte(payload), 0o600); err != nil {
			os.Exit(5)
		}
	case "fd-count":
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			os.Exit(9)
		}
		if err := os.WriteFile("/work/"+outputName, []byte(strconv.Itoa(len(entries))), 0o600); err != nil {
			os.Exit(5)
		}
	case "exit-125":
		os.Exit(125)
	case "exit-124":
		os.Exit(124)
	default:
		os.Exit(4)
	}
}

const expectedProfileSettings = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry">
 <item oor:path="/org.openoffice.Office.Common/Security/Scripting">
  <prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
  <prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop>
  <prop oor:name="DisableActiveContent" oor:op="fuse"><value>true</value></prop>
  <prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop>
 </item>
</oor:items>`

func retryMarkerMissing() bool {
	_, err := os.Stat("/work/profile/retry-marker")
	return errors.Is(err, os.ErrNotExist)
}

func markRetry() error {
	return os.WriteFile("/work/profile/retry-marker", []byte("retry"), 0o600)
}

func profileParentRetryMarkerMissing() bool {
	_, err := os.Stat("/work/retry-marker")
	return errors.Is(err, os.ErrNotExist)
}

func markProfileParentRetry() error {
	return os.WriteFile("/work/retry-marker", []byte("retry"), 0o600)
}

func writeRetryOutput() error {
	return os.WriteFile("/work/"+outputName, []byte("unexpected retry output"), 0o600)
}

func runReconstructedRetry() error {
	if retryMarkerMissing() {
		input, err := os.ReadFile("/work/source.docx")
		if err != nil || !bytes.Equal(input, []byte("input")) {
			return fmt.Errorf("initial input: %v", err)
		}
		settings, err := os.ReadFile("/work/profile/user/registrymodifications.xcu")
		if err != nil || !bytes.Equal(settings, []byte(expectedProfileSettings)) {
			return fmt.Errorf("initial profile: %v", err)
		}
		if err := os.WriteFile("/work/source.docx", []byte("mutated input"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile("/work/profile/user/registrymodifications.xcu", []byte("mutated settings"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile("/work/"+outputName, []byte("stale output"), 0o600); err != nil {
			return err
		}
		if err := markRetry(); err != nil {
			return err
		}
		os.Exit(81)
	}
	input, err := os.ReadFile("/work/source.docx")
	if err != nil || !bytes.Equal(input, []byte("input")) {
		return fmt.Errorf("restored input: %v", err)
	}
	settings, err := os.ReadFile("/work/profile/user/registrymodifications.xcu")
	if err != nil || !bytes.Equal(settings, []byte(expectedProfileSettings)) {
		return fmt.Errorf("restored profile: %v", err)
	}
	if _, err := os.Lstat("/work/" + outputName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stale output remains: %v", err)
	}
	return os.WriteFile("/work/"+outputName, []byte("fresh retried output"), 0o600)
}

func waitForFile(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}
